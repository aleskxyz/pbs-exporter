package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
)

// testModule builds a config file module for srv.
func testModule(t *testing.T, srv *fakeServer, mc ModuleConfig) *Module {
	t.Helper()
	mc.User, mc.TokenName, mc.TokenValue, mc.Endpoint = "mon@pbs", "exporter", "secret", srv.URL
	return testModules(t, map[string]ModuleConfig{"site": mc})["site"]
}

func testExporter(t *testing.T, srv *fakeServer, mc ModuleConfig) *Exporter {
	t.Helper()
	return NewExporter(context.Background(), testModule(t, srv, mc), 10*time.Second)
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

var allOK = map[string]float64{"apt": 1, "datastores": 1, "groups": 1, "jobs": 1, "node": 1, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1}

func collectorStatus(collectors map[string]float64) string {
	var b strings.Builder
	b.WriteString("# HELP pbs_collector_success Whether a collector succeeded in the last scrape.\n# TYPE pbs_collector_success gauge\n")
	for _, name := range []string{"apt", "datastores", "groups", "jobs", "node", "sync", "tasks", "time", "version", "zfs"} {
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
# HELP pbs_info Endpoint host of the scraped PBS, always 1.
# TYPE pbs_info gauge
pbs_info{endpoint_host="127.0.0.1"} 1
# HELP pbs_version_info Version of the PBS installation.
# TYPE pbs_version_info gauge
pbs_version_info{release="2",repoid="0123abcd",version="4.2"} 1
# HELP pbs_datastore_available_bytes The available bytes of the underlying storage of the datastore, as PBS reports them (may be negative when the storage is overcommitted).
# TYPE pbs_datastore_available_bytes gauge
pbs_datastore_available_bytes{datastore="store1"} 1000
# HELP pbs_datastore_used_bytes The used bytes of the underlying storage of the datastore.
# TYPE pbs_datastore_used_bytes gauge
pbs_datastore_used_bytes{datastore="store1"} 2000
# HELP pbs_node_load15 The load average of the node over 15 minutes.
# TYPE pbs_node_load15 gauge
pbs_node_load15 0.125
# HELP pbs_node_subscription_due_timestamp_seconds The next due date of the subscription of the node.
# TYPE pbs_node_subscription_due_timestamp_seconds gauge
pbs_node_subscription_due_timestamp_seconds 1893456000
`, "pbs_collector_success", "pbs_up", "pbs_info", "pbs_version_info", "pbs_datastore_available_bytes", "pbs_datastore_used_bytes",
		"pbs_node_load15", "pbs_node_subscription_due_timestamp_seconds")
}

// A failing endpoint only fails its own collector.
func TestCollectorFailureIsIsolated(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/status": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, collectorStatus(map[string]float64{"apt": 1, "datastores": 1, "groups": 1, "jobs": 1, "node": 0, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1})+`
# HELP pbs_up Was the last query of PBS successful (the version call).
# TYPE pbs_up gauge
pbs_up 1
# HELP pbs_datastore_available_bytes The available bytes of the underlying storage of the datastore, as PBS reports them (may be negative when the storage is overcommitted).
# TYPE pbs_datastore_available_bytes gauge
pbs_datastore_available_bytes{datastore="store1"} 1000
`, "pbs_collector_success", "pbs_up", "pbs_datastore_available_bytes", "pbs_node_cpu_usage_ratio")
	// the subscription has its own request and is still exported
	if n := testutil.CollectAndCount(e, "pbs_node_subscription_info"); n != 1 {
		t.Errorf("pbs_node_subscription_info: %d series, want 1", n)
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
	compare(t, e, collectorStatus(map[string]float64{"apt": 1, "datastores": 1, "groups": 0, "jobs": 1, "node": 1, "sync": 1, "tasks": 1, "time": 1, "version": 1, "zfs": 1}), "pbs_collector_success")
	if !strings.Contains(logs.String(), `required_privilege="Datastore.Audit on /datastore`) {
		t.Errorf("the log does not name the privilege:\n%s", logs)
	}
}

// The scrape timeout from Prometheus cuts off slow calls; the scrape still
// answers with pbs_up and the collector status.
func TestScrapeDeadlineFromHeader(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=site-a": {body: `{"data":[]}`, delay: 10 * time.Second},
	})
	h := newMetricsHandler(map[string]*Module{"site": testModule(t, srv, ModuleConfig{Timeout: ptr(Duration(time.Minute))})}, testTimeouts)
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
		`pbs_collector_success{collector="datastores"} 1`,
		`pbs_collector_success{collector="groups"} 0`,
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

// Store names and namespaces are escaped in the request URLs.
func TestRequestEscaping(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage":                              {body: `{"data":[{"store":"st&1 x","mount-status":"nonremovable"}]}`},
		"/api2/json/admin/datastore":                                     {body: `{"data":[{"store":"st&1 x"}]}`},
		"/api2/json/admin/datastore/st&1%20x/namespace":                  {body: `{"data":[{"ns":"a/b&c=d"}]}`},
		"/api2/json/admin/datastore/st&1%20x/snapshots?ns=a%2Fb%26c%3Dd": {body: `{"data":[{"backup-type":"vm","backup-id":"1","backup-time":1}]}`},
		"/api2/json/admin/datastore/st&1%20x/groups?ns=a%2Fb%26c%3Dd":    {body: `{"data":[{"backup-type":"vm","backup-id":"1","backup-count":1,"last-backup":1}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_group_snapshots Number of snapshots of the backup group (backup-count).
# TYPE pbs_group_snapshots gauge
pbs_group_snapshots{backup_id="1",backup_type="vm",datastore="st&1 x",datastore_namespace="a/b&c=d"} 1
`, "pbs_group_snapshots")
}

func TestDisabledCollector(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"groups": false}})
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
		overrides["/api2/json/admin/datastore/store1/groups?ns="+ns] = fakeResp{body: `{"data":[]}`, delay: 20 * time.Millisecond}
	}
	overrides["/api2/json/admin/datastore/store1/namespace"] = fakeResp{body: `{"data":[` + strings.Join(namespaces, ",") + `]}`}
	srv := newFakePBS(t, overrides)
	m := testModule(t, srv, ModuleConfig{MaxConcurrency: ptr(2)})

	done := make(chan struct{})
	for range 3 {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = testutil.CollectAndCount(NewExporter(context.Background(), m, 10*time.Second))
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
	cfg, err := ParseConfig([]byte("modules:\n  a:\n" + validModule + "    max_concurrency: 8\n    collectors:\n      zfs: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	modules, err := BuildModules(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := modules["a"]
	var names []string
	for _, c := range m.collectors {
		names = append(names, c.name)
	}
	if want := []string{"version", "node", "datastores", "groups", "jobs", "sync", "tasks", "time", "apt"}; !slices.Equal(names, want) || m.maxConcurrency != 8 {
		t.Errorf("collectors %v, max_concurrency %d", names, m.maxConcurrency)
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

// Every response is fetched once per scrape, although several collectors
// use it.
func TestSharedResponsesFetchedOnce(t *testing.T) {
	srv := newFakePBS(t, nil)
	// access also reads the permissions
	if n := testutil.CollectAndCount(testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true}})); n == 0 {
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

// gaugeValue returns the value of the only series of name.
func gaugeValue(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			if len(f.GetMetric()) != 1 {
				t.Fatalf("%s: %d series, want 1", name, len(f.GetMetric()))
			}
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	var names []string
	for _, f := range families {
		names = append(names, f.GetName())
	}
	t.Fatalf("%s not found in %s", name, strings.Join(names, ", "))
	return 0
}

// seriesOf returns the series of the given metrics of one scrape of c, one
// per line in the text format.
func seriesOf(t *testing.T, c prometheus.Collector, names ...string) []string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for _, f := range families {
		if slices.Contains(names, f.GetName()) {
			if _, err := expfmt.MetricFamilyToText(&buf, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func TestJoinUnique(t *testing.T) {
	a, b := errors.New("a"), errors.New("b")
	if got := joinUnique(nil, a, b, errors.New("a"), nil); got == nil || got.Error() != "a\nb" {
		t.Errorf("got %v", got)
	}
	if joinUnique(nil, nil) != nil {
		t.Error("nil errors must give nil")
	}
}

// An answer without data is an error, not an empty list or zero values.
func TestResponseWithoutData(t *testing.T) {
	for _, body := range []string{`{"data":null}`, `{}`} {
		srv := newFakePBS(t, map[string]fakeResp{
			"/api2/json/version":              {body: body},
			"/api2/json/nodes/localhost/time": {body: body},
		})
		got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_up", "pbs_version_info", "pbs_node_time_seconds", "pbs_collector_success")
		for _, want := range []string{"pbs_up 0", `pbs_collector_success{collector="time"} 0`, `pbs_collector_success{collector="version"} 0`} {
			if !slices.Contains(got, want) {
				t.Errorf("%s: missing %s in %v", body, want, got)
			}
		}
		if slices.ContainsFunc(got, func(l string) bool {
			return strings.HasPrefix(l, "pbs_version_info") || strings.HasPrefix(l, "pbs_node_time_seconds")
		}) {
			t.Errorf("%s: series without data: %v", body, got)
		}
	}
}
