package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestZFSMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_zfs_pool_healthy Whether the ZFS pool is ONLINE; health is the state PBS reports.
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
	// "time" has whole seconds, and the answer is written about 400ms after
	// the request arrived, with the midpoint 200ms after it was sent
	if got < 598 || got > 601.5 {
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
