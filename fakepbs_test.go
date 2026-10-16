package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeResp is the answer of the fake PBS to one route.
type fakeResp struct {
	// status defaults to 200
	status int
	body   string
	// delay before answering; the request context still cancels it
	delay time.Duration
}

// defaultRoutes maps the API paths to the fixtures in testdata/fakepbs. A key
// with "?" only matches that query.
var defaultRoutes = map[string]string{
	"/api2/json/version":                                    "version.json",
	"/api2/json/status/datastore-usage":                     "datastore-usage.json",
	"/api2/json/admin/datastore":                            "admin-datastore.json",
	"/api2/json/admin/datastore/store1/namespace":           "namespace.json",
	"/api2/json/admin/datastore/store1/snapshots?ns=":       "snapshots-root.json",
	"/api2/json/admin/datastore/store1/snapshots?ns=site-a": "snapshots-site-a.json",
	"/api2/json/admin/datastore/store1/groups?ns=":          "groups-root.json",
	"/api2/json/admin/datastore/store1/groups?ns=site-a":    "groups-site-a.json",
	"/api2/json/nodes/localhost/status":                     "node-status.json",
	"/api2/json/nodes/localhost/subscription":               "subscription.json",
}

// fakeServer is a fake PBS that answers from defaultRoutes and per-test
// overrides, and records the requests it received.
type fakeServer struct {
	*httptest.Server
	mu          sync.Mutex
	auths       []string
	requests    []string
	inFlight    int
	maxInFlight int
}

// newFakePBS starts a fake PBS that answers from defaultRoutes, replaced or
// extended by overrides.
func newFakePBS(t *testing.T, overrides map[string]fakeResp) *fakeServer {
	t.Helper()
	routes := loadDefaultRoutes(t)
	for route, resp := range overrides {
		routes[route] = resp
	}
	return startFakePBS(t, routes)
}

func loadDefaultRoutes(t *testing.T) map[string]fakeResp {
	t.Helper()
	routes := map[string]fakeResp{}
	for route, file := range defaultRoutes {
		body, err := os.ReadFile(filepath.Join("testdata", "fakepbs", file))
		if err != nil {
			t.Fatal(err)
		}
		routes[route] = fakeResp{body: string(body)}
	}
	return routes
}

func startFakePBS(t *testing.T, routes map[string]fakeResp) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.requests = append(f.requests, r.URL.RequestURI())
		f.inFlight++
		f.maxInFlight = max(f.maxInFlight, f.inFlight)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
		}()

		resp, ok := routes[r.URL.EscapedPath()+"?"+r.URL.RawQuery]
		if !ok {
			resp, ok = routes[r.URL.EscapedPath()]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if resp.delay > 0 {
			select {
			case <-time.After(resp.delay):
			case <-r.Context().Done():
				return
			}
		}
		if resp.status != 0 {
			w.WriteHeader(resp.status)
		}
		_, _ = fmt.Fprint(w, resp.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) Auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

func (f *fakeServer) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeServer) MaxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// fakePBS is a fake PBS that reports the given version and answers slowly, so
// that concurrent scrapes overlap. It returns the Authorization headers it
// received.
func fakePBS(t *testing.T, version string) (*fakeServer, func() []string) {
	t.Helper()
	routes := loadDefaultRoutes(t)
	routes["/api2/json/version"] = fakeResp{body: fmt.Sprintf(`{"data":{"version":%q,"release":"1","repoid":"abc"}}`, version)}
	for route, r := range routes {
		r.delay = 20 * time.Millisecond
		routes[route] = r
	}
	f := startFakePBS(t, routes)
	return f, f.Auths
}

var testTimeouts = scrapeTimeouts{fallback: 30 * time.Second, offset: 500 * time.Millisecond}
