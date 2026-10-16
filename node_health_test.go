package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestZFSMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_zfs_pool_healthy Whether the ZFS pool is ONLINE; health is the state PBS reports. PBS does not list pools it can not read the usage of (e.g. FAULTED or UNAVAIL), so watch for pools that disappear.
# TYPE pbs_zfs_pool_healthy gauge
pbs_zfs_pool_healthy{health="DEGRADED",pool="backup-pool"} 0
pbs_zfs_pool_healthy{health="ONLINE",pool="rpool"} 1
# HELP pbs_zfs_pool_size_bytes Size of the ZFS pool.
# TYPE pbs_zfs_pool_size_bytes gauge
pbs_zfs_pool_size_bytes{pool="backup-pool"} 2e+14
pbs_zfs_pool_size_bytes{pool="rpool"} 4.8e+11
# HELP pbs_zfs_pool_allocated_bytes Allocated space of the ZFS pool.
# TYPE pbs_zfs_pool_allocated_bytes gauge
pbs_zfs_pool_allocated_bytes{pool="backup-pool"} 5e+13
pbs_zfs_pool_allocated_bytes{pool="rpool"} 6e+09
# HELP pbs_zfs_pool_fragmentation_ratio Fragmentation of the free space of the ZFS pool (0-1).
# TYPE pbs_zfs_pool_fragmentation_ratio gauge
pbs_zfs_pool_fragmentation_ratio{pool="backup-pool"} 0.03
pbs_zfs_pool_fragmentation_ratio{pool="rpool"} 0.2
`, "pbs_zfs_pool_healthy", "pbs_zfs_pool_size_bytes", "pbs_zfs_pool_allocated_bytes", "pbs_zfs_pool_fragmentation_ratio")
}

// The offset uses the UTC "time" field: a node that runs 10 minutes fast in a
// UTC+1 zone has an offset of 600s, not 4200s. The fake answers after 400ms,
// which must not count as offset.
func TestNodeTimeOffset(t *testing.T) {
	now := time.Now().Unix() + 600
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/time": {
			body:  fmt.Sprintf(`{"data":{"time":%d,"localtime":%d,"timezone":"Europe/London\n"}}`, now, now+3600),
			delay: 400 * time.Millisecond,
		},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"snapshots": false, "groups": false}})
	got := gaugeValue(t, e, "pbs_node_time_offset_seconds")
	// "time" has whole seconds (taken at the start of the test, then +0.5s
	// as the expected middle of that second), the midpoint of the round trip
	// is 200ms after the request was sent
	if got < 599.2 || got > 600.4 {
		t.Errorf("offset = %.3fs, want about 600s", got)
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

// A node whose clock is in sync reports an offset of about 0 on average,
// although PBS cuts the time to whole seconds.
func TestNodeTimeOffsetUnbiased(t *testing.T) {
	srv := &fakeServer{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/nodes/localhost/time" {
			http.NotFound(w, r)
			return
		}
		// spread the requests over the second
		time.Sleep(time.Duration(rand.IntN(50)) * time.Millisecond)
		_, _ = fmt.Fprintf(w, `{"data":{"time":%d}}`, time.Now().Unix())
	}))
	t.Cleanup(srv.Close)
	m := testModule(t, srv, ModuleConfig{})
	var sum float64
	const n = 40
	for range n {
		s := &scrapeState{api: NewExporter(context.Background(), srv.URL, m, 5*time.Second).api, module: m}
		ch := make(chan prometheus.Metric, 1)
		if err := timeCollector.collect(context.Background(), s, ch); err != nil {
			t.Fatal(err)
		}
		var pb dto.Metric
		if err := (<-ch).Write(&pb); err != nil {
			t.Fatal(err)
		}
		sum += pb.GetGauge().GetValue()
		time.Sleep(time.Duration(rand.IntN(40)) * time.Millisecond)
	}
	if mean := sum / n; mean < -0.25 || mean > 0.25 {
		t.Errorf("mean offset of an in-sync clock = %.3fs, want about 0", mean)
	}
}
