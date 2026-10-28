package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestZFSMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_zfs_pool_health_info Health of the ZFS pool as PBS reports it (ONLINE, DEGRADED, ...), always 1. PBS does not list pools it can not read the usage of (e.g. FAULTED or UNAVAIL), so watch for pools that disappear.
# TYPE pbs_zfs_pool_health_info gauge
pbs_zfs_pool_health_info{health="DEGRADED",pool="backup-pool"} 1
pbs_zfs_pool_health_info{health="ONLINE",pool="rpool"} 1
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
`, "pbs_zfs_pool_health_info", "pbs_zfs_pool_size_bytes", "pbs_zfs_pool_allocated_bytes", "pbs_zfs_pool_fragmentation_ratio")
}

// The node time is the UTC "time" field, not "localtime", which is shifted by
// the time zone.
func TestNodeTime(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/time": {body: `{"data":{"time":1700000600,"localtime":1700004200,"timezone":"Europe/London"}}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"groups": false}})
	if got := gaugeValue(t, e, "pbs_node_time_seconds"); got != 1700000600 {
		t.Errorf("node time = %v, want the time PBS reports", got)
	}
}

// Values the node status does not report have no series, not 0: a short or
// missing loadavg, empty memory, swap and root file system.
func TestNodeStatusWithoutValues(t *testing.T) {
	for _, load := range []string{`"loadavg":[0.5],`, ``, `"loadavg":null,`} {
		srv := newFakePBS(t, map[string]fakeResp{
			"/api2/json/nodes/localhost/status": {body: `{"data":{` + load + `"cpu":0.1,"memory":{},"swap":{},"root":{}}}`},
		})
		e := testExporter(t, srv, ModuleConfig{})
		compare(t, e, collectorStatus(allOK), "pbs_collector_success")
		got := seriesOf(t, e, "pbs_node_load1", "pbs_node_load5", "pbs_node_load15", "pbs_node_memory_size_bytes",
			"pbs_node_swap_used_bytes", "pbs_node_rootfs_size_bytes", "pbs_node_uptime_seconds", "pbs_node_cpu_usage_ratio")
		if want := []string{"pbs_node_cpu_usage_ratio 0.1"}; !slices.Equal(got, want) {
			t.Errorf("%q: got %v, want %v", load, got, want)
		}
	}
}

func TestSubscriptionWithoutDueDate(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/subscription": {body: `{"data":{"status":"notfound"}}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_node_subscription_info The subscription info of the host.
# TYPE pbs_node_subscription_info gauge
pbs_node_subscription_info{productname="",status="notfound"} 1
`, "pbs_node_subscription_info", "pbs_node_subscription_due_timestamp_seconds")
}

// The pending updates come from the package list, the running kernel from
// the node status.
func TestAptMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_apt_updates_available Number of packages with a pending update, as of the last package list refresh on the node.
# TYPE pbs_apt_updates_available gauge
pbs_apt_updates_available 2
# HELP pbs_node_kernel_info The running kernel of the PBS node (current-kernel of /nodes/localhost/status), always 1.
# TYPE pbs_node_kernel_info gauge
pbs_node_kernel_info{release="6.8.12-4-pve"} 1
`, "pbs_apt_updates_available", "pbs_node_kernel_info")
}

// The disks collector exports the SMART status and wearout PBS reports; it is
// off by default.
func TestDisks(t *testing.T) {
	srv := newFakePBS(t, nil)
	if n := testutil.CollectAndCount(testExporter(t, srv, ModuleConfig{}), "pbs_disk_smart_status"); n != 0 {
		t.Errorf("disks must be off by default, got %d series", n)
	}

	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"disks": true}})
	compare(t, e, `
# HELP pbs_disk_smart_status SMART health of a disk as PBS reports it (status passed, failed or unknown), always 1.
# TYPE pbs_disk_smart_status gauge
pbs_disk_smart_status{disk="nvme0n1",model="Example NVMe 3.2TB",serial="N0001",status="unknown"} 1
pbs_disk_smart_status{disk="sda",model="Example SSD",serial="S0001",status="passed"} 1
# HELP pbs_disk_wearout_percent The wearout field PBS reports for the disk: the remaining rated life of an SSD in percent (100 = new); omitted when PBS reports none.
# TYPE pbs_disk_wearout_percent gauge
pbs_disk_wearout_percent{disk="sda"} 97
`, "pbs_disk_smart_status", "pbs_disk_wearout_percent")
}

// A disk without SMART status is exported with the status PBS reports: none.
func TestDiskWithoutStatus(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/disks/list": {body: `{"data":[{"name":"sdb","model":"M","serial":"S"}]}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"disks": true}}), "pbs_disk_smart_status")
	if want := []string{`pbs_disk_smart_status{disk="sdb",model="M",serial="S",status=""} 1`}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Without Sys.Audit on "/", PBS reports the subscription without product name
// and due date and without an error; the node collector reports it.
func TestFilteredSubscription(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":           {body: `{"data":{"/":{"Datastore.Audit":true},"/datastore":{"Datastore.Audit":true},"/system":{"Sys.Audit":true}}}`},
		"/api2/json/nodes/localhost/subscription": {body: `{"data":{"status":"active","message":"x","url":"y"}}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_node_subscription_info", "pbs_collector_success")
	for _, want := range []string{`pbs_node_subscription_info{productname="",status="active"} 1`, `pbs_collector_success{collector="node"} 0`} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if !strings.Contains(logs.String(), "missing privilege Sys.Audit on /") {
		t.Errorf("the log does not name the missing privilege:\n%s", logs)
	}
}
