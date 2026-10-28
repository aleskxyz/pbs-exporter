package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDatastoreExtras(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_datastore_estimated_full_timestamp_seconds When PBS expects the datastore to be full (estimated-full-date as PBS reports it; a date in the past means PBS has no estimate).
# TYPE pbs_datastore_estimated_full_timestamp_seconds gauge
pbs_datastore_estimated_full_timestamp_seconds{datastore="store1"} 4102444800
# HELP pbs_datastore_info Settings of the datastore, always 1.
# TYPE pbs_datastore_info gauge
pbs_datastore_info{backend_type="filesystem",datastore="store1",gc_schedule="daily",notification_mode="notification-system",verify_new="true"} 1
# HELP pbs_datastore_keep Retention setting of the datastore.
# TYPE pbs_datastore_keep gauge
pbs_datastore_keep{datastore="store1",keep="daily"} 7
pbs_datastore_keep{datastore="store1",keep="weekly"} 4
# HELP pbs_gc_disk_bytes Bytes used by chunks, as of the last garbage collection (0 before the first one).
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
// exported as PBS reports it; removable datastores report whether they are
// mounted.
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
# HELP pbs_datastore_estimated_full_timestamp_seconds When PBS expects the datastore to be full (estimated-full-date as PBS reports it; a date in the past means PBS has no estimate).
# TYPE pbs_datastore_estimated_full_timestamp_seconds gauge
pbs_datastore_estimated_full_timestamp_seconds{datastore="store1"} 1.462147431e+09
# HELP pbs_datastore_mounted Whether a removable datastore is mounted; omitted for other datastores.
# TYPE pbs_datastore_mounted gauge
pbs_datastore_mounted{datastore="usb1"} 1
pbs_datastore_mounted{datastore="usb2"} 0
# HELP pbs_datastore_maintenance Present while the datastore is in maintenance mode, always 1.
# TYPE pbs_datastore_maintenance gauge
pbs_datastore_maintenance{datastore="store1",mode="read-only"} 1
`, "pbs_datastore_estimated_full_timestamp_seconds", "pbs_datastore_mounted", "pbs_datastore_maintenance")
}

// A datastore without garbage collection so far has a gc-status of zeros;
// they are exported as PBS reports them.
func TestGCStatusWithoutGC(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[{"store":"store1","mount-status":"nonremovable",
			"gc-status":{"disk-bytes":0,"index-data-bytes":0,"pending-bytes":0,"removed-bytes":0,"still-bad":0,"removed-bad":0}}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	got := seriesOf(t, e, "pbs_gc_disk_bytes", "pbs_gc_bad_chunks")
	want := []string{
		`pbs_gc_bad_chunks{datastore="store1",state="removed_bad"} 0`,
		`pbs_gc_bad_chunks{datastore="store1",state="still_bad"} 0`,
		`pbs_gc_disk_bytes{datastore="store1"} 0`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Unmounted datastores and those in a maintenance mode that blocks reading
// are not scanned.
func TestDatastoreAvailability(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","mount-status":"nonremovable"},
			{"store":"usb","mount-status":"notmounted"},
			{"store":"offline","mount-status":"nonremovable"},
			{"store":"ro","mount-status":"nonremovable"}]}`},
		"/api2/json/admin/datastore": {body: `{"data":[
			{"store":"store1"},{"store":"usb"},
			{"store":"offline","maintenance":"type=offline,message=disk swap"},
			{"store":"ro","maintenance":"read-only"}]}`},
		"/api2/json/admin/datastore/ro/namespace":     {body: `{"data":[{"ns":""}]}`},
		"/api2/json/admin/datastore/ro/snapshots?ns=": {body: `{"data":[]}`},
		"/api2/json/admin/datastore/ro/groups?ns=":    {body: `{"data":[]}`},
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
	if want := []string{"ro", "store1"}; !slices.Equal(scanned, want) {
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

// PBS lists datastores it can not read the usage of without total, used and
// avail (unmounted, offline maintenance with "error", namespace access only).
// They must not be exported as 0 bytes. A negative avail is exported as PBS
// reports it.
func TestDatastoreWithoutUsage(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","avail":1000,"total":3000,"used":2000,"mount-status":"nonremovable"},
			{"store":"full","avail":-5,"total":100,"used":105,"mount-status":"nonremovable"},
			{"store":"usb","mount-status":"notmounted"},
			{"store":"offline","mount-status":"nonremovable","error":"datastore is in maintenance mode"}]}`},
		"/api2/json/admin/datastore": {body: `{"data":[{"store":"store1"},{"store":"full"},{"store":"usb"},{"store":"offline","maintenance":"type=offline"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"groups": false}})
	compare(t, e, `
# HELP pbs_datastore_available_bytes The available bytes of the underlying storage of the datastore, as PBS reports them (may be negative when the storage is overcommitted).
# TYPE pbs_datastore_available_bytes gauge
pbs_datastore_available_bytes{datastore="full"} -5
pbs_datastore_available_bytes{datastore="store1"} 1000
# HELP pbs_datastore_size_bytes The size of the underlying storage of the datastore in bytes.
# TYPE pbs_datastore_size_bytes gauge
pbs_datastore_size_bytes{datastore="full"} 100
pbs_datastore_size_bytes{datastore="store1"} 3000
`, "pbs_datastore_available_bytes", "pbs_datastore_size_bytes")
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="datastores"} 1`) {
		t.Errorf("datastores collector fails: %v", got)
	}
}

// pbs_datastore_error is 1 for a datastore PBS reports an error for instead of
// its usage (failed storage, offline maintenance) and 0 for the others, also
// unmounted ones and those the token only sees namespaces of; only a failure
// outside maintenance is logged as warning.
func TestDatastoreError(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","avail":1000,"total":3000,"used":2000,"mount-status":"nonremovable"},
			{"store":"usb","mount-status":"notmounted"},
			{"store":"offline","mount-status":"nonremovable","error":"datastore is in maintenance mode"},
			{"store":"broken","mount-status":"nonremovable","error":"unable to open chunk store at \"/mnt/broken/.chunks\" - EIO"},
			{"store":"nsonly","mount-status":"nonremovable"}]}`},
		"/api2/json/admin/datastore": {body: `{"data":[{"store":"store1"},{"store":"usb"},{"store":"offline","maintenance":"type=offline"},{"store":"broken"},{"store":"nsonly"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"groups": false}})
	compare(t, e, `
# HELP pbs_datastore_error Whether PBS reports an error for the datastore in /status/datastore-usage (e.g. it can not open it: failed storage, offline maintenance).
# TYPE pbs_datastore_error gauge
pbs_datastore_error{datastore="broken"} 1
pbs_datastore_error{datastore="nsonly"} 0
pbs_datastore_error{datastore="offline"} 1
pbs_datastore_error{datastore="store1"} 0
pbs_datastore_error{datastore="usb"} 0
`, "pbs_datastore_error")
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="datastores"} 1`) {
		t.Errorf("datastores collector fails: %v", got)
	}
	warnings := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "reports an error for the datastore") {
			warnings++
			if !strings.Contains(line, "datastore=broken") {
				t.Errorf("warning for a datastore in maintenance: %s", line)
			}
		}
	}
	if warnings == 0 {
		t.Errorf("no warning for the failed datastore:\n%s", logs)
	}
}

// Every error of a namespace is reported with the datastore and namespace,
// also one of a datastore that became unavailable during the scrape.
func TestNamespaceErrors(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=":       {status: http.StatusBadRequest, body: `{"data":null,"message":"datastore is in offline maintenance mode"}`},
		"/api2/json/admin/datastore/store1/snapshots?ns=site-a": {status: http.StatusForbidden, body: `{"data":null,"message":"permission check failed"}`},
	})
	logs := captureLogs(t)
	e := testExporter(t, srv, ModuleConfig{})
	testutil.CollectAndCount(e)
	for _, want := range []string{`datastore store1, namespace \"site-a\"`, `datastore store1, namespace \"\": /api2/json/admin/datastore/store1/snapshots: HTTP 400: datastore is in offline maintenance mode`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing error %s:\n%s", want, logs)
		}
	}
}

// A datastore PBS reports an error for (e.g. failed storage) is reported by
// pbs_datastore_error; the groups collector skips it instead of failing.
func TestGroupsSkipFailedDatastore(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {body: `{"data":[
			{"store":"store1","avail":1000,"total":3000,"used":2000,"mount-status":"nonremovable"},
			{"store":"broken","mount-status":"nonremovable","error":"unable to open chunk store at \"/mnt/broken/.chunks\" - EIO"}]}`},
		"/api2/json/admin/datastore": {body: `{"data":[{"store":"store1"},{"store":"broken"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="groups"} 1`) {
		t.Errorf("groups collector fails: %v", got)
	}
	for _, r := range srv.Requests() {
		if strings.Contains(r, "/datastore/broken/") {
			t.Errorf("the failed datastore was read: %s", r)
		}
	}
}

// An error listing the namespaces of a datastore names the datastore.
func TestNamespaceListError(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/namespace": {status: http.StatusForbidden, body: `{"data":null,"message":"permission check failed"}`},
	})
	testutil.CollectAndCount(testExporter(t, srv, ModuleConfig{}))
	if !strings.Contains(logs.String(), "datastore store1: /api2/json/admin/datastore/store1/namespace: HTTP 403") {
		t.Errorf("the error does not name the datastore:\n%s", logs)
	}
}

// Without the maintenance modes (/admin/datastore), which datastores can be
// read is unknown: the groups collector fails and reads no datastore.
func TestGroupsWithoutMaintenanceList(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
	})
	e := testExporter(t, srv, ModuleConfig{})
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="groups"} 0`) {
		t.Errorf("groups collector does not fail: %v", got)
	}
	if n := testutil.CollectAndCount(e, "pbs_group_snapshots"); n != 0 {
		t.Errorf("%d group series without the maintenance modes", n)
	}
	for _, r := range srv.Requests() {
		if strings.HasSuffix(r, "/namespace") {
			t.Errorf("datastore read without its maintenance mode: %s", r)
		}
	}
}

// The maintenance modes and the datastore settings do not depend on the usage
// endpoint: they are exported when it fails.
func TestDatastoresWithoutUsage(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/status/datastore-usage": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
		"/api2/json/admin/datastore":        {body: `{"data":[{"store":"store1","backend-type":"filesystem","maintenance":"type=read-only"}]}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_datastore_maintenance", "pbs_datastore_info", "pbs_datastore_size_bytes", "pbs_collector_success")
	for _, want := range []string{
		`pbs_datastore_maintenance{datastore="store1",mode="read-only"} 1`,
		`pbs_collector_success{collector="datastores"} 0`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if !slices.ContainsFunc(got, func(l string) bool {
		return strings.HasPrefix(l, `pbs_datastore_info{backend_type="filesystem",datastore="store1"`)
	}) {
		t.Errorf("no pbs_datastore_info with the backend of /admin/datastore: %v", got)
	}
}
