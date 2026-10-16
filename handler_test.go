package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// scrape is also called from goroutines, so it must not call t.Fatal.
func scrape(t *testing.T, h http.Handler, query string) (int, string) {
	t.Helper()
	return scrapeRequest(t, h, httptest.NewRequest("GET", "/metrics?"+query, nil))
}

func scrapeRequest(t *testing.T, h http.Handler, req *http.Request) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Error(err)
	}
	return rec.Code, string(body)
}

func testModules(t *testing.T, cfg Config, fallback *ModuleConfig) map[string]*Module {
	t.Helper()
	m, err := BuildModules(cfg, fallback, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModulesUseTheirOwnCredentials(t *testing.T) {
	srvA, authsA := fakePBS(t, "1.1.1")
	srvB, authsB := fakePBS(t, "2.2.2")
	h := newMetricsHandler(testModules(t, Config{
		"site-a": {User: "root@pam", TokenName: "ta", TokenValue: "sa", Endpoint: srvA.URL},
		"site-b": {User: "mon@pbs", TokenName: "tb", TokenValue: "sb", Endpoint: srvB.URL},
	}, nil), true, testTimeouts)

	for _, tc := range []struct {
		module, version string
		auths           func() []string
		want            string
	}{
		{"site-a", "1.1.1", authsA, "PBSAPIToken=root@pam!ta:sa"},
		{"site-b", "2.2.2", authsB, "PBSAPIToken=mon@pbs!tb:sb"},
	} {
		code, body := scrape(t, h, "module="+tc.module)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.module, code)
		}
		if !strings.Contains(body, "\npbs_up 1\n") {
			t.Errorf("%s: expected pbs_up 1, got:\n%s", tc.module, body)
		}
		if !strings.Contains(body, `version="`+tc.version+`"`) {
			t.Errorf("%s: expected version %s in output", tc.module, tc.version)
		}
		got := tc.auths()
		if len(got) == 0 {
			t.Fatalf("%s: no request reached the fake PBS", tc.module)
		}
		for _, a := range got {
			if a != tc.want {
				t.Errorf("%s: Authorization = %q, want %q", tc.module, a, tc.want)
			}
		}
	}
}

func TestUnknownModule(t *testing.T) {
	h := newMetricsHandler(testModules(t, Config{"a": {User: "u", TokenName: "n", TokenValue: "v", Endpoint: "http://127.0.0.1:1"}}, nil), true, testTimeouts)
	code, _ := scrape(t, h, "module=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	// no "default" module when the config file defines none and there is no legacy fallback
	if code, _ := scrape(t, h, ""); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestLegacyDefaultModule(t *testing.T) {
	srv, auths := fakePBS(t, "3.3.3")
	h := newMetricsHandler(testModules(t, nil, &ModuleConfig{User: "root@pam", TokenName: "pbs-exporter", TokenValue: "s"}), false, testTimeouts)
	// no module parameter -> "default" built from flags/env, target from URL
	code, body := scrape(t, h, "target="+srv.URL)
	if code != http.StatusOK || !strings.Contains(body, "\npbs_up 1\n") {
		t.Fatalf("status %d body:\n%s", code, body)
	}
	if got := auths(); len(got) == 0 || got[0] != "PBSAPIToken=root@pam!pbs-exporter:s" {
		t.Errorf("unexpected Authorization headers: %v", got)
	}
}

// Config file modules get their defaults from the file, never from the flags:
// no "default" module unless defined, 5s timeout, TLS verification on.
func TestConfigModuleDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pbs.yaml")
	if err := os.WriteFile(path, []byte("site:\n  user: file@pbs\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007/\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, modules, err := LoadModules(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || len(cfg) != 1 {
		t.Fatalf("modules = %v, want only the config file module", moduleNames(modules))
	}
	if _, ok := modules[defaultModuleName]; ok {
		t.Error("no default module must be built when the config file defines none")
	}
	m := modules["site"]
	if m.authorizationHeader != "PBSAPIToken=file@pbs!n:v" || m.endpoint != "https://pbs:8007" {
		t.Errorf("unexpected module: %+v", m)
	}
	if m.client.Timeout != defaultModuleTimeout {
		t.Errorf("timeout = %s, want %s", m.client.Timeout, defaultModuleTimeout)
	}
	if m.client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Error("config file modules must verify TLS unless insecure is set")
	}
}

// A config file module must pin an endpoint, so the "target" parameter can
// never decide where its token is sent; the legacy module keeps accepting any
// target, as before.
func TestConfigModulesRequireEndpoint(t *testing.T) {
	if _, err := BuildModules(Config{"site": {User: "u@pbs", TokenName: "n", TokenValue: "v"}}, nil, time.Second); err == nil {
		t.Error("expected an error for a config file module without endpoint")
	}

	pinned, _ := fakePBS(t, "4.4.4")
	other, otherAuths := fakePBS(t, "8.8.8")
	file := newMetricsHandler(testModules(t, Config{"site": {User: "u@pbs", TokenName: "n", TokenValue: "SECRET", Endpoint: pinned.URL}}, nil), true, testTimeouts)
	for _, target := range []string{other.URL, "ftp://127.0.0.1:1", "http://127.0.0.1:1%0aINFO:%20forged"} {
		code, body := scrape(t, file, "module=site&target="+target)
		if code != http.StatusOK || !strings.Contains(body, `version="4.4.4"`) {
			t.Errorf("target %q: status %d, want the pinned endpoint to be scraped, body:\n%s", target, code, body)
		}
	}
	if got := otherAuths(); len(got) != 0 {
		t.Errorf("token was sent to the target parameter: %v", got)
	}

	legacy := newMetricsHandler(testModules(t, nil, &ModuleConfig{User: "u@pam", TokenName: "n", TokenValue: "v"}), false, testTimeouts)
	if code, _ := scrape(t, legacy, "target=http://127.0.0.1:1/api2"); code != http.StatusOK {
		t.Errorf("legacy target: status = %d, want 200", code)
	}
}

func TestValidateBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://pbs:8007":   "https://pbs:8007",
		"https://pbs:8007/":  "https://pbs:8007",
		"http://10.0.0.1":    "http://10.0.0.1",
		"https://[::1]:8007": "https://[::1]:8007",
	} {
		if got, err := validateBaseURL(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "pbs:8007", "https://", "https://:8007", "file:///etc/passwd", "https://pbs:8007/x", "https://pbs:8007?a", "https://pbs:8007#", "https://u@pbs:8007", "https://pbs\n:8007"} {
		if _, err := validateBaseURL(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

// Upstream requests stop when the scrape request is cancelled, instead of
// running until the module timeout.
func TestScrapeCancelStopsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	h := newMetricsHandler(testModules(t, Config{"site": {User: "u@pbs", TokenName: "n", TokenValue: "v", Endpoint: srv.URL, Timeout: "1m"}}, nil), true, testTimeouts)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, body := scrapeRequest(t, h, httptest.NewRequest("GET", "/metrics?module=site", nil).WithContext(ctx))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("scrape took %s after the request was cancelled", d)
	}
	if !strings.Contains(body, "\npbs_up 0\n") {
		t.Errorf("expected pbs_up 0, got:\n%s", body)
	}
}

func TestModuleEndpointIsPinned(t *testing.T) {
	pinned, pinnedAuths := fakePBS(t, "9.9.9")
	other, otherAuths := fakePBS(t, "0.0.0")
	h := newMetricsHandler(testModules(t, Config{
		"site": {User: "u@pbs", TokenName: "n", TokenValue: "v", Endpoint: pinned.URL},
	}, nil), true, testTimeouts)
	_, body := scrape(t, h, "module=site&target="+other.URL)
	if !strings.Contains(body, `version="9.9.9"`) {
		t.Errorf("expected the pinned PBS to be scraped, got:\n%s", body)
	}
	if len(pinnedAuths()) == 0 || len(otherAuths()) != 0 {
		t.Errorf("target parameter must be ignored when the module pins an endpoint (pinned=%d other=%d requests)", len(pinnedAuths()), len(otherAuths()))
	}
}

// Concurrent scrapes of different modules must not mix up their metrics.
func TestConcurrentScrapes(t *testing.T) {
	srvA, _ := fakePBS(t, "1.1.1")
	srvB, _ := fakePBS(t, "2.2.2")
	h := newMetricsHandler(testModules(t, Config{
		"a": {User: "u@pbs", TokenName: "n", TokenValue: "a", Endpoint: srvA.URL},
		"b": {User: "u@pbs", TokenName: "n", TokenValue: "b", Endpoint: srvB.URL},
	}, nil), true, testTimeouts)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		for _, tc := range []struct{ module, version, other string }{
			{"a", "1.1.1", "2.2.2"},
			{"b", "2.2.2", "1.1.1"},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, body := scrape(t, h, "module="+tc.module)
				if code != http.StatusOK {
					t.Errorf("module %s: status %d", tc.module, code)
				}
				if !strings.Contains(body, `version="`+tc.version+`"`) || strings.Contains(body, `version="`+tc.other+`"`) {
					t.Errorf("module %s: wrong or mixed metrics:\n%s", tc.module, body)
				}
			}()
		}
	}
	wg.Wait()
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
default:
  user: root@pam
  token_name: t
  token_value: v
  endpoint: https://pbs:8007
  insecure: true
  timeout: 10s
`))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg["default"]; m.User != "root@pam" || m.Endpoint != "https://pbs:8007" || !m.Insecure || m.Timeout != "10s" {
		t.Errorf("unexpected module: %+v", m)
	}

	// a valid module body, so that each case below is invalid for one reason only
	const body = "  user: u\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007\n"
	const mod = "a:\n" + body
	for name, in := range map[string]string{
		"empty":            ``,
		"not a map":        `- a`,
		"missing token":    "a:\n  user: u\n  token_name: n\n  endpoint: https://pbs:8007\n",
		"missing endpoint": "a:\n  user: u\n  token_name: n\n  token_value: v\n",
		"unknown key":      mod + "  verify_ssl: false\n",
		"two documents":    mod + "---\nb:\n  user: u\n",
		"empty name":       "\"\":\n" + body,
		"duplicate":        mod + mod,
		"zero timeout":     mod + "  timeout: 0s\n",
		"neg timeout":      mod + "  timeout: -1s\n",
		"bad timeout":      mod + "  timeout: soon\n",
		"bad endpoint":     "a:\n  user: u\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007/api2\n",
		"second doc":       mod + "---\n~\n---\nb: 1\n",
		"null key":         "~:\n" + body + "b:\n" + body,
		"null word key":    "null:\n" + body,
		"newline name":     "\"a\\nINFO: forged\":\n" + body,
		"cr name key":      "\"a\\r\":\n" + body,
		"block token":      "a:\n  user: u\n  token_name: n\n  endpoint: https://pbs:8007\n  token_value: |\n    v\n",
		"tab user":         "a:\n  user: \"u\\tx\"\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007\n",
		"cr name":          "a:\n  user: u\n  token_name: \"n\\r\"\n  token_value: v\n  endpoint: https://pbs:8007\n",
		"del token":        "a:\n  user: u\n  token_name: n\n  token_value: \"v\\x7f\"\n  endpoint: https://pbs:8007\n",
	} {
		if _, err := ParseConfig([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := NewModule("x", ModuleConfig{Timeout: "soon"}, time.Second); err == nil {
		t.Error("expected an error for an invalid timeout")
	}
	if _, err := NewModule("x", ModuleConfig{Timeout: "0s"}, time.Second); err == nil {
		t.Error("expected an error for a zero timeout")
	}

	// an empty trailing document is fine
	for _, in := range []string{
		mod + "---\n",
		"---\n" + mod + "---\n...\n",
	} {
		if _, err := ParseConfig([]byte(in)); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}

	// the endpoint is stored without the trailing slash
	cfg, err = ParseConfig([]byte("a:\n  user: u\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg["a"].Endpoint; got != "https://pbs:8007" {
		t.Errorf("endpoint = %q, want it without the trailing slash", got)
	}
}

// A config file path with a line break is rejected before it can reach a log
// line. The check runs before the file is read, so no file is needed (such a
// name could not be created on Windows anyway).
func TestLoadConfigRejectsControlCharsInPath(t *testing.T) {
	for _, path := range []string{"pbs\nINFO: forged.yaml", "pbs\r.yaml"} {
		_, err := LoadConfig(filepath.Join(t.TempDir(), path))
		if err == nil || err.Error() != "config file path must not contain control characters" {
			t.Errorf("%q: got error %v, want the control character error", path, err)
		}
	}
}

// saveFlags restores the flag values applyEnv may change after the test.
func saveFlags(t *testing.T) {
	t.Helper()
	ptrs := []*string{configFile, loglevel, endpoint, username, apitokenname, apitoken, timeout, insecure, maxConcurrency, collectorList, taskLookback, taskLimit, metricsPath, listenAddress, scrapeTimeout, scrapeTimeoutOffset}
	saved := make([]string, len(ptrs))
	for i, p := range ptrs {
		saved[i] = *p
	}
	t.Cleanup(func() {
		for i, p := range ptrs {
			*p = saved[i]
		}
	})
}

func setConnectionEnv(t *testing.T) {
	t.Helper()
	userFile := filepath.Join(t.TempDir(), "username")
	if err := os.WriteFile(userFile, []byte("file-user@pam\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PBS_ENDPOINT", "https://legacy:8007")
	t.Setenv("PBS_USERNAME_FILE", userFile)
	t.Setenv("PBS_API_TOKEN", "legacy-secret")
	t.Setenv("PBS_TIMEOUT", "1m")
	t.Setenv("PBS_INSECURE", "true")
	t.Setenv("PBS_METRICS_PATH", "/m")
}

// With a config file, the connection environment variables are not read at all.
func TestApplyEnvConfigMode(t *testing.T) {
	saveFlags(t)
	setConnectionEnv(t)
	t.Setenv("PBS_CONFIG_FILE", "/etc/pbs.yaml")
	// would make ReadSecretFile exit the test binary if it was read
	t.Setenv("PBS_API_TOKEN_NAME_FILE", "/nonexistent")
	before := []string{*endpoint, *username, *apitokenname, *apitoken, *timeout, *insecure}

	applyEnv()

	if *configFile != "/etc/pbs.yaml" {
		t.Errorf("configFile = %q", *configFile)
	}
	if *metricsPath != "/m" {
		t.Errorf("metricsPath = %q, the non-connection environment variables must still apply", *metricsPath)
	}
	after := []string{*endpoint, *username, *apitokenname, *apitoken, *timeout, *insecure}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("connection setting changed from %q to %q with a config file", before[i], after[i])
		}
	}
}

// Without a config file, the connection environment variables apply as before.
func TestApplyEnvLegacyMode(t *testing.T) {
	saveFlags(t)
	setConnectionEnv(t)
	t.Setenv("PBS_CONFIG_FILE", "")

	applyEnv()

	if *configFile != "" || *endpoint != "https://legacy:8007" || *username != "file-user@pam" ||
		*apitoken != "legacy-secret" || *timeout != "1m" || *insecure != "true" || *metricsPath != "/m" {
		t.Errorf("unexpected settings: configFile=%q endpoint=%q username=%q apitoken=%q timeout=%q insecure=%q metricsPath=%q",
			*configFile, *endpoint, *username, *apitoken, *timeout, *insecure, *metricsPath)
	}
}

// Without a config file, the "module" parameter is ignored and PBS_ENDPOINT
// pins the default module, as before modules existed.
func TestLegacyModeUnchanged(t *testing.T) {
	pinned, pinnedAuths := fakePBS(t, "5.5.5")
	other, otherAuths := fakePBS(t, "6.6.6")
	h := newMetricsHandler(testModules(t, nil, &ModuleConfig{User: "root@pam", TokenName: "n", TokenValue: "v", Endpoint: pinned.URL}), false, testTimeouts)

	code, body := scrape(t, h, "module=foo&target="+other.URL)
	if code != http.StatusOK || !strings.Contains(body, `version="5.5.5"`) {
		t.Fatalf("status %d, want 200 from the pinned endpoint, body:\n%s", code, body)
	}
	if len(pinnedAuths()) == 0 || len(otherAuths()) != 0 {
		t.Errorf("PBS_ENDPOINT must pin the default module (pinned=%d other=%d requests)", len(pinnedAuths()), len(otherAuths()))
	}
	for _, series := range []string{"\ngo_goroutines ", "\npromhttp_metric_handler_requests_total{"} {
		if !strings.Contains(body, series) {
			t.Errorf("missing %q in the output", strings.TrimSpace(series))
		}
	}

	// PBS_TIMEOUT=0 still means no timeout
	if m, err := BuildModules(nil, &ModuleConfig{User: "u", TokenName: "n", TokenValue: "v"}, 0); err != nil || m[defaultModuleName].client.Timeout != 0 {
		t.Errorf("timeout 0 must be accepted for the legacy module: %v", err)
	}
}

// redirector answers every request with a redirect to target.
func redirector(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Config file modules do not follow redirects, so the token stays with the
// configured endpoint; the legacy module still follows them, as before.
func TestRedirects(t *testing.T) {
	other, otherAuths := fakePBS(t, "7.7.7")
	pinned := redirector(t, other.URL)

	file := newMetricsHandler(testModules(t, Config{"site": {User: "u@pbs", TokenName: "n", TokenValue: "SECRET", Endpoint: pinned.URL}}, nil), true, testTimeouts)
	_, body := scrape(t, file, "module=site")
	if !strings.Contains(body, "\npbs_up 0\n") {
		t.Errorf("expected pbs_up 0 for a redirect, got:\n%s", body)
	}
	if got := otherAuths(); len(got) != 0 {
		t.Fatalf("token was forwarded to the redirect target: %v", got)
	}

	legacy := newMetricsHandler(testModules(t, nil, &ModuleConfig{User: "u@pam", TokenName: "n", TokenValue: "v", Endpoint: pinned.URL}), false, testTimeouts)
	if _, body := scrape(t, legacy, ""); !strings.Contains(body, `version="7.7.7"`) {
		t.Errorf("legacy module must still follow redirects, got:\n%s", body)
	}
}

// A metric that can not be gathered does not cost the other metrics.
func TestGatherErrorKeepsOtherMetrics(t *testing.T) {
	srv, _ := fakePBS(t, "1.2.3")
	m := testModules(t, Config{"site": {User: "u@pbs", TokenName: "n", TokenValue: "v", Endpoint: srv.URL}}, nil)["site"]
	// a collector that emits the same series twice
	dup := &collector{name: "dup", enabled: true, descs: []*prometheus.Desc{version},
		collect: func(_ context.Context, _ *scrapeState, ch chan<- prometheus.Metric) error {
			for range 2 {
				ch <- prometheus.MustNewConstMetric(version, prometheus.GaugeValue, 1, "x", "y", "z")
			}
			return nil
		}}
	m.collectors = []*collector{versionCollector, dup}
	code, body := scrape(t, newMetricsHandler(map[string]*Module{"site": m}, true, testTimeouts), "module=site")
	if code != http.StatusOK || !strings.Contains(body, "\npbs_up 1\n") {
		t.Errorf("status %d, body:\n%s", code, body)
	}
}
