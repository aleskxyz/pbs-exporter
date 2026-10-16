package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

// testModules builds modules through the same validation and merging as a
// config file without defaults.
func testModules(t *testing.T, modules map[string]ModuleConfig) map[string]*Module {
	t.Helper()
	cfg, err := NewConfig(ServerConfig{}, ModuleConfig{}, modules)
	if err != nil {
		t.Fatal(err)
	}
	m, err := BuildModules(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModulesUseTheirOwnCredentials(t *testing.T) {
	srvA, authsA := fakePBS(t, "1.1.1")
	srvB, authsB := fakePBS(t, "2.2.2")
	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{
		"site-a": {User: "root@pam", TokenName: "ta", TokenValue: "sa", Endpoint: srvA.URL},
		"site-b": {User: "mon@pbs", TokenName: "tb", TokenValue: "sb", Endpoint: srvB.URL},
	}), testTimeouts)

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
		// the exporter's own metrics are exposed as well
		for _, series := range []string{"\ngo_goroutines ", "\npromhttp_metric_handler_requests_total{"} {
			if !strings.Contains(body, series) {
				t.Errorf("missing %q in the output", strings.TrimSpace(series))
			}
		}
	}
}

// Without "module", the module named "default" is used, if there is one.
func TestDefaultModule(t *testing.T) {
	srv, _ := fakePBS(t, "3.3.3")
	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{
		"default": {User: "u@pbs", TokenName: "n", TokenValue: "v", Endpoint: srv.URL},
	}), testTimeouts)
	if code, body := scrape(t, h, ""); code != http.StatusOK || !strings.Contains(body, `version="3.3.3"`) {
		t.Errorf("status %d, body:\n%s", code, body)
	}
}

func TestUnknownModule(t *testing.T) {
	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{"a": {User: "u", TokenName: "n", TokenValue: "v", Endpoint: "http://127.0.0.1:1"}}), testTimeouts)
	code, _ := scrape(t, h, "module=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	// no "default" module when the config file defines none
	if code, _ := scrape(t, h, ""); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// A module always scrapes its endpoint: the "target" parameter of earlier
// versions can never decide where its token is sent.
func TestModuleEndpointIsPinned(t *testing.T) {
	pinned, pinnedAuths := fakePBS(t, "9.9.9")
	other, otherAuths := fakePBS(t, "0.0.0")
	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{
		"site": {User: "u@pbs", TokenName: "n", TokenValue: "SECRET", Endpoint: pinned.URL},
	}), testTimeouts)
	for _, target := range []string{other.URL, "ftp://127.0.0.1:1", "http://127.0.0.1:1%0aINFO:%20forged", "http://%FF:8007"} {
		code, body := scrape(t, h, "module=site&target="+target)
		if code != http.StatusOK || !strings.Contains(body, `version="9.9.9"`) {
			t.Errorf("target %q: status %d, want the pinned endpoint to be scraped, body:\n%s", target, code, body)
		}
	}
	if len(pinnedAuths()) == 0 || len(otherAuths()) != 0 {
		t.Errorf("the target parameter must be ignored (pinned=%d other=%d requests)", len(pinnedAuths()), len(otherAuths()))
	}
}

// Upstream requests stop when the scrape request is cancelled, instead of
// running until the module timeout.
func TestScrapeCancelStopsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{
		"site": {User: "u@pbs", TokenName: "n", TokenValue: "v", Endpoint: srv.URL, Timeout: ptr(Duration(time.Minute))},
	}), testTimeouts)

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

// Concurrent scrapes of different modules must not mix up their metrics.
func TestConcurrentScrapes(t *testing.T) {
	srvA, _ := fakePBS(t, "1.1.1")
	srvB, _ := fakePBS(t, "2.2.2")
	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{
		"a": {User: "u@pbs", TokenName: "n", TokenValue: "a", Endpoint: srvA.URL},
		"b": {User: "u@pbs", TokenName: "n", TokenValue: "b", Endpoint: srvB.URL},
	}), testTimeouts)

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

// redirector answers every request with a redirect to target.
func redirector(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Modules do not follow redirects, so the token stays with the endpoint.
func TestRedirects(t *testing.T) {
	other, otherAuths := fakePBS(t, "7.7.7")
	pinned := redirector(t, other.URL)

	h := newMetricsHandler(testModules(t, map[string]ModuleConfig{"site": {User: "u@pbs", TokenName: "n", TokenValue: "SECRET", Endpoint: pinned.URL}}), testTimeouts)
	_, body := scrape(t, h, "module=site")
	if !strings.Contains(body, "\npbs_up 0\n") {
		t.Errorf("expected pbs_up 0 for a redirect, got:\n%s", body)
	}
	if got := otherAuths(); len(got) != 0 {
		t.Fatalf("token was forwarded to the redirect target: %v", got)
	}
}

// A metric that can not be gathered does not cost the other metrics.
func TestGatherErrorKeepsOtherMetrics(t *testing.T) {
	srv, _ := fakePBS(t, "1.2.3")
	m := testModules(t, map[string]ModuleConfig{"site": {User: "u@pbs", TokenName: "n", TokenValue: "v", Endpoint: srv.URL}})["site"]
	// a collector that emits the same series twice
	dup := &collector{name: "dup", enabled: true, descs: []*prometheus.Desc{version},
		collect: func(_ context.Context, _ *scrapeState, ch chan<- prometheus.Metric) error {
			for range 2 {
				ch <- prometheus.MustNewConstMetric(version, prometheus.GaugeValue, 1, "x", "y", "z")
			}
			return nil
		}}
	m.collectors = []*collector{versionCollector, dup}
	code, body := scrape(t, newMetricsHandler(map[string]*Module{"site": m}, testTimeouts), "module=site")
	if code != http.StatusOK || !strings.Contains(body, "\npbs_up 1\n") {
		t.Errorf("status %d, body:\n%s", code, body)
	}
}
