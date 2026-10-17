package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDatastoreExtras(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_datastore_estimated_full_timestamp_seconds When PBS expects the datastore to be full; omitted without an estimate or when it is in the past.
# TYPE pbs_datastore_estimated_full_timestamp_seconds gauge
pbs_datastore_estimated_full_timestamp_seconds{datastore="store1"} 4102444800
# HELP pbs_datastore_info Settings of the datastore, always 1.
# TYPE pbs_datastore_info gauge
pbs_datastore_info{backend_type="filesystem",datastore="store1",gc_schedule="daily",notification_mode="notification-system",verify_new="true"} 1
# HELP pbs_datastore_keep Retention setting of the datastore.
# TYPE pbs_datastore_keep gauge
pbs_datastore_keep{datastore="store1",keep="daily"} 7
pbs_datastore_keep{datastore="store1",keep="weekly"} 4
# HELP pbs_gc_disk_bytes Bytes used by chunks, as of the last garbage collection.
# TYPE pbs_gc_disk_bytes gauge
pbs_gc_disk_bytes{datastore="store1"} 1e+06
# HELP pbs_gc_index_data_bytes Bytes referenced by indexes, as of the last garbage collection (divided by the disk bytes: deduplication factor).
# TYPE pbs_gc_index_data_bytes gauge
pbs_gc_index_data_bytes{datastore="store1"} 5e+06
# HELP pbs_gc_pending_bytes Bytes waiting for removal by the next garbage collection.
# TYPE pbs_gc_pending_bytes gauge
pbs_gc_pending_bytes{datastore="store1"} 100
# HELP pbs_gc_removed_bytes Bytes removed by the last garbage collection.
# TYPE pbs_gc_removed_bytes gauge
pbs_gc_removed_bytes{datastore="store1"} 2000
# HELP pbs_gc_bad_chunks Corrupt chunks found by the last garbage collection: still_bad are still referenced by snapshots, removed_bad were removed.
# TYPE pbs_gc_bad_chunks gauge
pbs_gc_bad_chunks{datastore="store1",state="removed_bad"} 1
pbs_gc_bad_chunks{datastore="store1",state="still_bad"} 0
`, "pbs_datastore_estimated_full_timestamp_seconds", "pbs_datastore_mounted", "pbs_datastore_maintenance", "pbs_datastore_info",
		"pbs_datastore_keep", "pbs_gc_disk_bytes", "pbs_gc_index_data_bytes", "pbs_gc_pending_bytes", "pbs_gc_removed_bytes", "pbs_gc_bad_chunks")
}

// An estimate in the past (PBS reports one from 2016 on some servers) is
// omitted; removable datastores report whether they are mounted.
func TestDatastoreMountAndMaintenance(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","mount-status":"nonremovable","estimated-full-date":1462147431},
			{"store":"usb1","mount-status":"mounted"},
			{"store":"usb2","mount-status":"notmounted"}]}`},
		"/api2/json/admin/datastore":                {body: `{"data":[{"store":"store1","maintenance":"type=read-only,message=migration"},{"store":"usb1"},{"store":"usb2"}]}`},
		"/api2/json/admin/datastore/usb1/namespace": {body: `{"data":[]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_datastore_mounted Whether a removable datastore is mounted; omitted for other datastores.
# TYPE pbs_datastore_mounted gauge
pbs_datastore_mounted{datastore="usb1"} 1
pbs_datastore_mounted{datastore="usb2"} 0
# HELP pbs_datastore_maintenance Present while the datastore is in maintenance mode, always 1.
# TYPE pbs_datastore_maintenance gauge
pbs_datastore_maintenance{datastore="store1",mode="read-only"} 1
`, "pbs_datastore_estimated_full_timestamp_seconds", "pbs_datastore_mounted", "pbs_datastore_maintenance")
}

func TestTaskFailures(t *testing.T) {
	now := time.Now().Unix()
	task := func(workerType, workerID string, ago int64, status string) string {
		return fmt.Sprintf(`{"upid":"UPID:pbs:1:2:3:0:%s:x:root@pam:","worker_type":%q,"worker_id":%q,"user":"root@pam","starttime":%d,"endtime":%d,"status":%q}`,
			workerType, workerType, workerID, now-ago-10, now-ago, status)
	}
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/tasks": {body: `{"data":[` + strings.Join([]string{
			task("backup", "store1:vm/100", 3600, "backup failed: connection reset"),
			task("backup", "store1:vm/101", 7200, "job failed"),
			task("backup", "store1:vm/102", 30*3600, "too old"),
			task("verificationjob", "store1:v-1", 3600, "WARNINGS: 2"),
			task("syncjob", "pbs-b:main:store2::s-1", 3600, "connection refused"),
			task("sync", "store3", 3600, "remote unreachable"),
			task("aptupdate", "", 3600, "apt-get update failed"),
			task("reader", "store1:vm/100", 3600, "OK"),
		}, ",") + `]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_task_failures Tasks that ended with an error in the last 24 hours, or task_lookback if shorter (warnings do not count); omitted when there are none.
# TYPE pbs_task_failures gauge
pbs_task_failures{datastore="",worker_type="aptupdate"} 1
pbs_task_failures{datastore="store1",worker_type="backup"} 2
pbs_task_failures{datastore="store2",worker_type="syncjob"} 1
pbs_task_failures{datastore="store3",worker_type="sync"} 1
`, "pbs_task_failures")
}

// The running kernel 6.8.12-4 is older than the installed 6.8.12-9; the
// meta package and a not installed kernel do not count.
func TestAptMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_apt_updates_available Number of packages with a pending update, as of the last package list refresh on the node.
# TYPE pbs_apt_updates_available gauge
pbs_apt_updates_available 2
# HELP pbs_reboot_required Whether a newer kernel than the running one is installed.
# TYPE pbs_reboot_required gauge
pbs_reboot_required 1
`, "pbs_apt_updates_available", "pbs_reboot_required")

	srv = newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/status": {body: `{"data":{"current-kernel":{"release":"6.8.12-9-pve"},"memory":{},"swap":{},"root":{}}}`},
	})
	compare(t, testExporter(t, srv, ModuleConfig{}), `
# HELP pbs_reboot_required Whether a newer kernel than the running one is installed.
# TYPE pbs_reboot_required gauge
pbs_reboot_required 0
`, "pbs_reboot_required")
}

func TestCompareKernelVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"6.8.12-10", "6.8.12-9", 1},
		{"6.8.12-9", "6.8.12-10", -1},
		{"7.0.14-12", "6.8.12-15", 1},
		{"6.8.12-4", "6.8.12-4", 0},
	} {
		if got := compareKernelVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("%s vs %s: got %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	// a backport kernel (opt-in kernel of PBS 3.4) is newer than 6.8
	if got := newestInstalledKernel([]aptPackage{
		{"proxmox-kernel-6.8.12-11-pve-signed", "6.8.12-11"},
		{"proxmox-kernel-6.14.8-2-bpo12-pve-signed", "6.14.8-2"},
		{"proxmox-kernel-6.14.11-4-bpo12-pve-signed", "6.14.11-4"},
	}); got != "6.14.11-4-bpo12" || compareKernelVersions(got, "6.14.8-2-bpo12") <= 0 {
		t.Errorf("newest installed backport kernel %q", got)
	}
	if got := newestInstalledKernel([]aptPackage{
		{"pve-kernel-5.15.30-2-pve", "5.15.30-2"},
		{"proxmox-kernel-6.2.16-20-pve", "6.2.16-20"},
		{"proxmox-kernel-6.8", "6.8.12-4"},
	}); got != "6.2.16-20" {
		t.Errorf("newest installed kernel %q", got)
	}
}

// access and disks are off by default; comments of users and tokens are
// never exported.
func TestAccessAndDisks(t *testing.T) {
	now := time.Now().Unix()
	reader := func(user string, ago int64) string {
		return fmt.Sprintf(`{"upid":"x","worker_type":"reader","worker_id":"store1:vm/100","user":%q,"starttime":%d,"endtime":%d,"status":"OK"}`, user, now-ago, now-ago+5)
	}
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/nodes/localhost/tasks": {body: `{"data":[` + strings.Join([]string{
			reader("sync@pbs", 3600), reader("sync@pbs", 7200), reader("alice@pbs", 60), reader("old@pbs", 30*3600),
		}, ",") + `]}`},
	})
	if n := testutil.CollectAndCount(testExporter(t, srv, ModuleConfig{}), "pbs_user_info", "pbs_disk_smart_status"); n != 0 {
		t.Errorf("access and disks must be off by default, got %d series", n)
	}

	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true, "disks": true}})
	compare(t, e, `
# HELP pbs_user_info A user of the PBS, always 1.
# TYPE pbs_user_info gauge
pbs_user_info{enabled="true",userid="mon@pbs"} 1
pbs_user_info{enabled="false",userid="old@pbs"} 1
pbs_user_info{enabled="true",userid="root@pam"} 1
# HELP pbs_user_expiry_timestamp_seconds When the user account expires; omitted when it never expires.
# TYPE pbs_user_expiry_timestamp_seconds gauge
pbs_user_expiry_timestamp_seconds{userid="mon@pbs"} 1.9e+09
# HELP pbs_token_expiry_timestamp_seconds When the API token expires; omitted when it never expires.
# TYPE pbs_token_expiry_timestamp_seconds gauge
pbs_token_expiry_timestamp_seconds{tokenid="mon@pbs!exporter"} 1.8e+09
# HELP pbs_reader_tasks Reader tasks (restores, and syncs from other servers) started in the last 24 hours, or task_lookback if shorter, per user or API token.
# TYPE pbs_reader_tasks gauge
pbs_reader_tasks{userid="alice@pbs"} 1
pbs_reader_tasks{userid="sync@pbs"} 2
# HELP pbs_disk_smart_status SMART health of a disk as PBS reports it (status passed, failed or unknown), always 1.
# TYPE pbs_disk_smart_status gauge
pbs_disk_smart_status{disk="nvme0n1",model="Example NVMe 3.2TB",serial="N0001",status="unknown"} 1
pbs_disk_smart_status{disk="sda",model="Example SSD",serial="S0001",status="passed"} 1
# HELP pbs_disk_wearout_ratio Used up share of the SSD's rated life (0 = new, 1 = worn out), as the PBS web interface shows it; omitted when PBS reports none.
# TYPE pbs_disk_wearout_ratio gauge
pbs_disk_wearout_ratio{disk="sda"} 0.03
`, "pbs_user_info", "pbs_user_expiry_timestamp_seconds", "pbs_token_expiry_timestamp_seconds", "pbs_reader_tasks",
		"pbs_disk_smart_status", "pbs_disk_wearout_ratio")

	h := newMetricsHandler(map[string]*Module{"site": testModule(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true}})}, true, testTimeouts)
	code, body := scrape(t, h, "module=site")
	if code != http.StatusOK || strings.Contains(body, "hunter2") || strings.Contains(body, "token secret") {
		t.Errorf("status %d, or a comment was exported:\n%s", code, body)
	}
}

// A datastore without garbage collection so far has a gc-status of zeros
// and no UPID; no GC series are exported for it.
func TestGCStatusWithoutGC(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[{"store":"store1","mount-status":"nonremovable",
			"gc-status":{"disk-bytes":0,"index-data-bytes":0,"pending-bytes":0,"removed-bytes":0,"still-bad":0,"removed-bad":0}}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	if got := seriesOf(t, e, "pbs_gc_disk_bytes", "pbs_gc_index_data_bytes", "pbs_gc_bad_chunks"); len(got) != 0 {
		t.Errorf("GC series without a garbage collection: %v", got)
	}
}

// The datastore of a task is taken from its worker ID per worker type.
func TestTaskStore(t *testing.T) {
	for _, tc := range []struct{ workerType, workerID, want string }{
		{"backup", "store1:vm/100", "store1"},
		{"prune", "store1:ns:vm/100", "store1"},
		{"prunejob", "store1", "store1"},
		{"verificationjob", "store1:v-1", "store1"},
		{"reader", "store1:vm/100", "store1"},
		{"syncjob", "remote:rs:store1::s-1", "store1"},
		{"sync", "store1", "store1"},
		{"unmount-device", "usb", "usb"},
		{"move-group", "store1:ns/vm/100:target", "store1"},
		{"tape-backup-job", "store1:pool:drive:job", "store1"},
		{"aptupdate", "", ""},
		{"termproxy", "1234", ""},
	} {
		if got := taskStore(taskItem{WorkerType: tc.workerType, WorkerID: tc.workerID}); got != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.workerType, tc.workerID, got, tc.want)
		}
	}
}

// Without Sys.Audit on /access/users, PBS lists only the token's own user;
// the access collector reports it.
func TestFilteredUserList(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {body: `{"data":{"/":{"Datastore.Audit":true},"/access":{"Sys.Audit":false},"/system":{"Sys.Audit":true}}}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true}}), "pbs_collector_success")
	if !slices.Contains(got, `pbs_collector_success{collector="access"} 0`) {
		t.Errorf("access collector does not report the filtered user list:\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(logs.String(), "missing privilege Sys.Audit on /access/users") {
		t.Errorf("no log about the missing privilege:\n%s", logs)
	}
}

// A running backport kernel compares with the installed ones; a PBS without
// current-kernel gives no series.
func TestRebootRequiredKernels(t *testing.T) {
	versions := `{"data":[{"Package":"proxmox-kernel-6.14.8-2-bpo12-pve-signed","OldVersion":"6.14.8-2"},
		{"Package":"proxmox-kernel-6.14.11-4-bpo12-pve-signed","OldVersion":"6.14.11-4"}]}`
	for name, tc := range map[string]struct {
		status string
		want   []string
	}{
		"older backport running": {`{"data":{"current-kernel":{"release":"6.14.8-2-bpo12-pve"},"memory":{},"swap":{},"root":{}}}`, []string{"pbs_reboot_required 1"}},
		"newest running":         {`{"data":{"current-kernel":{"release":"6.14.11-4-bpo12-pve"},"memory":{},"swap":{},"root":{}}}`, []string{"pbs_reboot_required 0"}},
		"no current-kernel":      {`{"data":{"memory":{},"swap":{},"root":{}}}`, nil},
	} {
		srv := newFakePBS(t, map[string]fakeResp{
			"/api2/json/nodes/localhost/status":       {body: tc.status},
			"/api2/json/nodes/localhost/apt/versions": {body: versions},
		})
		if got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_reboot_required"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
