package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testModule builds a config file module for srv.
func testModule(t *testing.T, srv *fakeServer, mc ModuleConfig) *Module {
	t.Helper()
	mc.User, mc.TokenName, mc.TokenValue, mc.Endpoint = "mon@pbs", "exporter", "secret", srv.URL
	return testModules(t, Config{"site": mc}, nil)["site"]
}

func testExporter(t *testing.T, srv *fakeServer, mc ModuleConfig) *Exporter {
	t.Helper()
	return NewExporter(context.Background(), srv.URL, testModule(t, srv, mc), 10*time.Second)
}

// compare checks the given metrics of one scrape against the exposition text.
func compare(t *testing.T, e *Exporter, expected string, names ...string) {
	t.Helper()
	if err := testutil.CollectAndCompare(e, strings.NewReader(expected), names...); err != nil {
		t.Error(err)
	}
}

// captureLogs collects the log output of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

var allOK = map[string]float64{"apt": 1, "datastores": 1, "groups": 1, "jobs": 1, "node": 1, "snapshots": 1, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1}

func collectorStatus(collectors map[string]float64) string {
	var b strings.Builder
	b.WriteString("# HELP pbs_collector_success Whether a collector succeeded in the last scrape.\n# TYPE pbs_collector_success gauge\n")
	for _, name := range []string{"apt", "datastores", "groups", "jobs", "node", "snapshots", "sync", "tasks", "time", "version", "zfs"} {
		if v, ok := collectors[name]; ok {
			fmt.Fprintf(&b, "pbs_collector_success{collector=%q} %g\n", name, v)
		}
	}
	return b.String()
}

func TestScrapeValues(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, collectorStatus(allOK)+`
# HELP pbs_up Was the last query of PBS successful (the version call).
# TYPE pbs_up gauge
pbs_up 1
# HELP pbs_info Module and endpoint host of the scraped PBS.
# TYPE pbs_info gauge
pbs_info{endpoint_host="127.0.0.1",module="site"} 1
# HELP pbs_version Version of the PBS installation.
# TYPE pbs_version gauge
pbs_version{release="2",repoid="0123abcd",version="4.2"} 1
# HELP pbs_datastore_available_bytes The available bytes of the underlying storage of the datastore.
# TYPE pbs_datastore_available_bytes gauge
pbs_datastore_available_bytes{datastore="store1"} 1000
# HELP pbs_datastore_used_bytes The used bytes of the underlying storage of the datastore.
# TYPE pbs_datastore_used_bytes gauge
pbs_datastore_used_bytes{datastore="store1"} 2000
# HELP pbs_snapshot_count The total number of backups of a namespace.
# TYPE pbs_snapshot_count gauge
pbs_snapshot_count{datastore="store1",namespace_root="",pbs_namespace=""} 3
pbs_snapshot_count{datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1
# HELP pbs_host_load15 The load for 15 minutes of the host.
# TYPE pbs_host_load15 gauge
pbs_host_load15 0.125
# HELP pbs_host_subscription_due_timestamp_seconds The subscription next due timestamp (unix seconds) of the host.
# TYPE pbs_host_subscription_due_timestamp_seconds gauge
pbs_host_subscription_due_timestamp_seconds{productname="Proxmox Backup Server Basic"} 1893456000
`, "pbs_collector_success", "pbs_up", "pbs_info", "pbs_version", "pbs_datastore_available_bytes", "pbs_datastore_used_bytes",
		"pbs_snapshot_count", "pbs_host_load15", "pbs_host_subscription_due_timestamp_seconds")
}

// A failing endpoint only fails its own collector.
func TestCollectorFailureIsIsolated(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/status": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, collectorStatus(map[string]float64{"apt": 1, "datastores": 1, "groups": 1, "jobs": 1, "node": 0, "snapshots": 1, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1})+`
# HELP pbs_up Was the last query of PBS successful (the version call).
# TYPE pbs_up gauge
pbs_up 1
# HELP pbs_datastore_available_bytes The available bytes of the underlying storage of the datastore.
# TYPE pbs_datastore_available_bytes gauge
pbs_datastore_available_bytes{datastore="store1"} 1000
`, "pbs_collector_success", "pbs_up", "pbs_datastore_available_bytes", "pbs_host_cpu_usage_ratio")
	// apt reads the running kernel from the same response, but leaves the
	// failure to the node collector; its subscription is still exported
	if n := testutil.CollectAndCount(e, "pbs_host_subscription_info"); n != 1 {
		t.Errorf("pbs_host_subscription_info: %d series, want 1", n)
	}
	if !strings.Contains(logs.String(), "/api2/json/nodes/localhost/status: HTTP 500: boom") {
		t.Errorf("the log does not name path, status and message:\n%s", logs)
	}
}

func TestForbiddenNamesPrivilege(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/namespace": {status: http.StatusForbidden, body: `{"data":null,"message":"permission check failed"}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, collectorStatus(map[string]float64{"apt": 1, "datastores": 1, "groups": 0, "jobs": 1, "node": 1, "snapshots": 0, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1}), "pbs_collector_success")
	if !strings.Contains(logs.String(), `required_privilege="Datastore.Audit on /datastore"`) {
		t.Errorf("the log does not name the privilege:\n%s", logs)
	}
}

// The scrape timeout from Prometheus cuts off slow calls; the scrape still
// answers with pbs_up and the collector status.
func TestScrapeDeadlineFromHeader(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=site-a": {body: `{"data":[]}`, delay: 10 * time.Second},
	})
	h := newMetricsHandler(map[string]*Module{"site": testModule(t, srv, ModuleConfig{Timeout: "1m"})}, true, testTimeouts)
	req := httptest.NewRequest("GET", "/metrics?module=site", nil)
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "1.5")

	start := time.Now()
	code, body := scrapeRequest(t, h, req)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("scrape took %s, want about 1s", d)
	}
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		"\npbs_up 1\n",
		`pbs_collector_success{collector="snapshots"} 0`,
		`pbs_collector_success{collector="datastores"} 1`,
		`pbs_collector_success{collector="groups"} 0`,
		`pbs_snapshot_count{datastore="store1",namespace_root="",pbs_namespace=""} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestScrapeTimeout(t *testing.T) {
	timeouts := scrapeTimeouts{fallback: 30 * time.Second, offset: 500 * time.Millisecond}
	for header, want := range map[string]time.Duration{
		"":      30 * time.Second,
		"10":    9500 * time.Millisecond,
		"2.5":   2 * time.Second,
		"0.4":   200 * time.Millisecond, // shorter than the offset: halved
		"1e-12": 30 * time.Second,       // rounds to 0
		"0":     30 * time.Second,
		"-1":    30 * time.Second,
		"NaN":   30 * time.Second,
		"+Inf":  30 * time.Second,
		"soon":  30 * time.Second,
		"1e300": 30 * time.Second,
	} {
		req := httptest.NewRequest("GET", "/metrics", nil)
		if header != "" {
			req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", header)
		}
		if got := timeouts.scrapeTimeout(req); got != want {
			t.Errorf("%q: got %s, want %s", header, got, want)
		}
	}
}

func TestShortLoadavg(t *testing.T) {
	for _, load := range []string{`"loadavg":[0.5],`, ``, `"loadavg":null,`} {
		srv := newFakePBS(t, map[string]fakeResp{
			"/api2/json/nodes/localhost/status": {body: `{"data":{` + load + `"cpu":0.1,"memory":{},"swap":{},"root":{}}}`},
		})
		e := testExporter(t, srv, ModuleConfig{})
		compare(t, e, collectorStatus(allOK), "pbs_collector_success")
		if n := testutil.CollectAndCount(e, "pbs_host_load1", "pbs_host_load5", "pbs_host_load15"); n != 0 {
			t.Errorf("%q: %d load series, want none", load, n)
		}
	}
}

// Store names and namespaces are escaped in the request URLs.
func TestRequestEscaping(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage":                              {body: `{"data":[{"store":"st&1 x","mount-status":"nonremovable"}]}`},
		"/api2/json/admin/datastore":                                     {body: `{"data":[{"store":"st&1 x"}]}`},
		"/api2/json/admin/datastore/st&1%20x/namespace":                  {body: `{"data":[{"ns":"a/b&c=d"}]}`},
		"/api2/json/admin/datastore/st&1%20x/snapshots?ns=a%2Fb%26c%3Dd": {body: `{"data":[{"backup-type":"vm","backup-id":"1","backup-time":1}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_snapshot_count The total number of backups of a namespace.
# TYPE pbs_snapshot_count gauge
pbs_snapshot_count{datastore="st&1 x",namespace_root="a",pbs_namespace="a/b&c=d"} 1
`, "pbs_snapshot_count")
}

func TestSubscriptionWithoutDueDate(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/subscription": {body: `{"data":{"status":"notfound"}}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_host_subscription_info The subscription info of the host.
# TYPE pbs_host_subscription_info gauge
pbs_host_subscription_info{productname="unknown",status="notfound"} 1
`, "pbs_host_subscription_info", "pbs_host_subscription_due_timestamp_seconds")
}

// Unmounted datastores and those in a maintenance mode that blocks reading
// are not scanned; a datastore that is being deleted is skipped without error.
func TestDatastoreAvailability(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","mount-status":"nonremovable"},
			{"store":"usb","mount-status":"notmounted"},
			{"store":"offline","mount-status":"nonremovable"},
			{"store":"ro","mount-status":"nonremovable"},
			{"store":"gone","mount-status":"nonremovable"}]}`},
		"/api2/json/admin/datastore": {body: `{"data":[
			{"store":"store1"},{"store":"usb"},
			{"store":"offline","maintenance":"type=offline,message=disk swap"},
			{"store":"ro","maintenance":"read-only"},
			{"store":"gone"}]}`},
		"/api2/json/admin/datastore/ro/namespace":     {body: `{"data":[{"ns":""}]}`},
		"/api2/json/admin/datastore/ro/snapshots?ns=": {body: `{"data":[]}`},
		"/api2/json/admin/datastore/ro/groups?ns=":    {body: `{"data":[]}`},
		"/api2/json/admin/datastore/gone/namespace":   {status: http.StatusBadRequest, body: `{"data":null,"message":"datastore is being deleted"}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, collectorStatus(allOK), "pbs_collector_success")

	var scanned []string
	for _, r := range srv.Requests() {
		if store, ok := strings.CutPrefix(r, "/api2/json/admin/datastore/"); ok && strings.HasSuffix(store, "/namespace") {
			scanned = append(scanned, strings.TrimSuffix(store, "/namespace"))
		}
	}
	slices.Sort(scanned)
	if want := []string{"gone", "ro", "store1"}; !slices.Equal(scanned, want) {
		t.Errorf("scanned datastores %v, want %v", scanned, want)
	}
	// PBS reports no usage for these datastores: no series instead of 0 bytes
	if n := testutil.CollectAndCount(e, "pbs_datastore_size_bytes"); n != 0 {
		t.Errorf("pbs_datastore_size_bytes: %d series, want none", n)
	}
}

func TestMaintenanceType(t *testing.T) {
	for in, want := range map[string]string{
		"type=offline,message=x":        "offline",
		`message="a, b",type=read-only`: "read-only",
		`message="x, y"`:                "",
		`offline,message="a,b"`:         "offline",
		"message=x,type=read-only":      "read-only",
		"offline":                       "offline",
		"offline,message=x":             "offline",
		"":                              "",
	} {
		if got := maintenanceType(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestDisabledCollector(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"snapshots": false, "groups": false}})
	compare(t, e, collectorStatus(map[string]float64{"apt": 1, "datastores": 1, "jobs": 1, "node": 1, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1}), "pbs_collector_success")
	for _, r := range srv.Requests() {
		if strings.Contains(r, "/namespace") || strings.Contains(r, "/snapshots") || strings.Contains(r, "/groups") {
			t.Errorf("disabled collector made request %s", r)
		}
	}
}

// The requests of all scrapes of a module are limited by max_concurrency.
func TestMaxConcurrency(t *testing.T) {
	overrides := map[string]fakeResp{}
	var namespaces []string
	for i := range 12 {
		ns := fmt.Sprintf("ns%d", i)
		namespaces = append(namespaces, fmt.Sprintf(`{"ns":%q}`, ns))
		overrides["/api2/json/admin/datastore/store1/snapshots?ns="+ns] = fakeResp{body: `{"data":[]}`, delay: 20 * time.Millisecond}
	}
	overrides["/api2/json/admin/datastore/store1/namespace"] = fakeResp{body: `{"data":[` + strings.Join(namespaces, ",") + `]}`}
	srv := newFakePBS(t, overrides)
	m := testModule(t, srv, ModuleConfig{MaxConcurrency: 2})

	done := make(chan struct{})
	for range 3 {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = testutil.CollectAndCount(NewExporter(context.Background(), srv.URL, m, 10*time.Second))
		}()
	}
	for range 3 {
		<-done
	}
	// the requests run concurrently, but never more than the limit
	if got := srv.MaxInFlight(); got != 2 {
		t.Errorf("%d concurrent requests, want 2", got)
	}
}

func TestCollectorConfig(t *testing.T) {
	const body = "  user: u\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007\n"
	cfg, err := ParseConfig([]byte("a:\n" + body + "  max_concurrency: 8\n  collectors:\n    snapshots: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	modules, err := BuildModules(cfg, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m := modules["a"]
	var names []string
	for _, c := range m.collectors {
		names = append(names, c.name)
	}
	if want := []string{"version", "node", "datastores", "groups", "jobs", "sync", "tasks", "zfs", "time", "apt"}; !slices.Equal(names, want) || m.maxConcurrency != 8 {
		t.Errorf("collectors %v, max_concurrency %d", names, m.maxConcurrency)
	}

	for name, in := range map[string]string{
		"unknown collector": "a:\n" + body + "  collectors:\n    nope: true\n",
		"disabled version":  "a:\n" + body + "  collectors:\n    version: false\n",
		"neg concurrency":   "a:\n" + body + "  max_concurrency: -1\n",
		"collectors list":   "a:\n" + body + "  collectors: [node]\n",
		"bad lookback":      "a:\n" + body + "  task_lookback: 8d\n",
		"zero lookback":     "a:\n" + body + "  task_lookback: 0s\n",
		"neg task limit":    "a:\n" + body + "  task_limit: -1\n",
	} {
		if _, err := ParseConfig([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	if got, err := parseCollectorList(" node , -snapshots,"); err != nil || len(got) != 2 || !got["node"] || got["snapshots"] {
		t.Errorf("parseCollectorList: %v, %v", got, err)
	}
	for _, in := range []string{"bogus", "-version", "-"} {
		if _, err := parseCollectorList(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

func TestErrText(t *testing.T) {
	err := errors.Join(errors.New("a: failed"), errors.New("b: failed\r\nforged"))
	if got, want := errText(err), "a: failed; b: failed; forged"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAPIError(t *testing.T) {
	long := strings.Repeat("x", 300)
	for body, want := range map[string]string{
		`{"data":null,"message":"permission check failed\n"}`: "permission check failed",
		"plain\r\ntext":                          "plaintext",
		`{"data":null,"message":"` + long + `"}`: strings.Repeat("x", maxErrorMessage) + "…",
		"":                                       "",
	} {
		if got := errorMessage([]byte(body)); got != want {
			t.Errorf("%q: got %q, want %q", body, got, want)
		}
	}

	err := fmt.Errorf("wrapped: %w", &APIError{Path: "/api2/json/version", Status: 403, Message: "permission check failed"})
	if !IsForbidden(err) || IsForbidden(errors.New("other")) {
		t.Error("IsForbidden does not detect a wrapped 403")
	}
	if got, want := err.Error(), "wrapped: /api2/json/version: HTTP 403: permission check failed"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDatastoreUnavailableMessages(t *testing.T) {
	for msg, want := range map[string]bool{
		"datastore is being deleted":               true,
		"datastore is being unmounted":             true,
		"datastore is in offline maintenance mode": true,
		"datastore 'usb' is not mounted":           true,
		"permission check failed":                  false,
	} {
		err := &APIError{Path: "/x", Status: http.StatusBadRequest, Message: msg}
		if got := isDatastoreUnavailable(err); got != want {
			t.Errorf("%q: got %t, want %t", msg, got, want)
		}
	}
	if isDatastoreUnavailable(&APIError{Path: "/x", Status: http.StatusInternalServerError, Message: "datastore is being deleted"}) {
		t.Error("only a 400 tells that a datastore is unavailable")
	}
}

// In legacy mode, the "target" parameter selects the PBS: scrapes of a slow
// PBS must not use up the request limit of the scrapes of other targets.
func TestLegacyTargetsDoNotShareRequestLimit(t *testing.T) {
	slowRoutes := loadDefaultRoutes(t)
	for route, r := range slowRoutes {
		r.delay = 3 * time.Second
		slowRoutes[route] = r
	}
	slow := startFakePBS(t, slowRoutes)
	fast := newFakePBS(t, nil)
	h := newMetricsHandler(testModules(t, nil, &ModuleConfig{User: "u@pam", TokenName: "n", TokenValue: "v", MaxConcurrency: 2}), false, testTimeouts)

	for range 2 {
		go func() {
			req := httptest.NewRequest("GET", "/metrics?target="+slow.URL, nil)
			req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "4")
			scrapeRequest(t, h, req)
		}()
	}
	time.Sleep(200 * time.Millisecond)
	req := httptest.NewRequest("GET", "/metrics?target="+fast.URL, nil)
	req.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "2")
	_, body := scrapeRequest(t, h, req)
	if !strings.Contains(body, "\npbs_up 1\n") || regexp.MustCompile(`pbs_collector_success\{[^}]*\} 0`).MatchString(body) {
		t.Errorf("the fast target was slowed down by the slow one:\n%s", body)
	}
}

// A target with invalid UTF-8 in the host gives pbs_up 0, not HTTP 500.
func TestInvalidUTF8Target(t *testing.T) {
	h := newMetricsHandler(testModules(t, nil, &ModuleConfig{User: "u@pam", TokenName: "n", TokenValue: "v"}), false, testTimeouts)
	code, body := scrape(t, h, "target=http://%FF:8007")
	if code != http.StatusOK || !strings.Contains(body, "\npbs_up 0\n") {
		t.Errorf("status %d, body:\n%s", code, body)
	}
}

// PBS lists datastores it can not read the usage of without total, used and
// avail (unmounted, offline maintenance with "error", namespace access only).
// They must not be exported as 0 bytes.
func TestDatastoreWithoutUsage(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","avail":1000,"total":3000,"used":2000,"mount-status":"nonremovable"},
			{"store":"usb","mount-status":"notmounted"},
			{"store":"offline","mount-status":"nonremovable","error":"datastore is in maintenance mode"}]}`},
		"/api2/json/admin/datastore": {body: `{"data":[{"store":"store1"},{"store":"usb"},{"store":"offline","maintenance":"type=offline"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_datastore_size_bytes The size of the underlying storage of the datastore in bytes.
# TYPE pbs_datastore_size_bytes gauge
pbs_datastore_size_bytes{datastore="store1"} 3000
`, "pbs_datastore_size_bytes")
	compare(t, e, collectorStatus(allOK), "pbs_collector_success")
}

// Every response is fetched once per scrape, although several collectors
// use it.
func TestSharedResponsesFetchedOnce(t *testing.T) {
	srv := newFakePBS(t, nil)
	if n := testutil.CollectAndCount(testExporter(t, srv, ModuleConfig{})); n == 0 {
		t.Fatal("no metrics")
	}
	counts := map[string]int{}
	for _, r := range srv.Requests() {
		counts[r]++
	}
	for r, n := range counts {
		if n != 1 {
			t.Errorf("%s requested %d times in one scrape", r, n)
		}
	}
}

// A datastore that becomes unavailable after its namespaces were listed is
// skipped, not reported as failure; other errors name the namespace.
func TestNamespaceErrors(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=":       {status: http.StatusBadRequest, body: `{"data":null,"message":"datastore is in offline maintenance mode"}`},
		"/api2/json/admin/datastore/store1/snapshots?ns=site-a": {status: http.StatusForbidden, body: `{"data":null,"message":"permission check failed"}`},
	})
	logs := captureLogs(t)
	e := testExporter(t, srv, ModuleConfig{})
	if n := testutil.CollectAndCount(e, "pbs_snapshot_count"); n != 0 {
		t.Errorf("pbs_snapshot_count: %d series, want none", n)
	}
	if !strings.Contains(logs.String(), `datastore store1, namespace \"site-a\"`) {
		t.Errorf("the error does not name the namespace:\n%s", logs)
	}
	if strings.Contains(logs.String(), "offline maintenance mode: ") {
		t.Errorf("an unavailable datastore must be skipped:\n%s", logs)
	}
}
