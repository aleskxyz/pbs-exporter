package main

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
)

// Groups are told apart by type and ID (vm/100 and ct/100 are two groups), a
// group without snapshots has no timestamp or verification series, and the
// notes are only on pbs_group_info.
func TestGroupMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_group_snapshots Number of snapshots of the backup group.
# TYPE pbs_group_snapshots gauge
pbs_group_snapshots{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 1
pbs_group_snapshots{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 2
pbs_group_snapshots{backup_id="benchmark",backup_type="host",datastore="store1",namespace_root="",pbs_namespace=""} 0
pbs_group_snapshots{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1
# HELP pbs_group_last_backup_timestamp_seconds Time of the newest finished snapshot of the backup group.
# TYPE pbs_group_last_backup_timestamp_seconds gauge
pbs_group_last_backup_timestamp_seconds{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 1700000000
pbs_group_last_backup_timestamp_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1700086400
pbs_group_last_backup_timestamp_seconds{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1700000000
# HELP pbs_group_last_backup_verify_state Verification state (ok, failed, none) of the newest finished snapshot of the backup group, always 1.
# TYPE pbs_group_last_backup_verify_state gauge
pbs_group_last_backup_verify_state{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace="",state="ok"} 1
pbs_group_last_backup_verify_state{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace="",state="none"} 1
pbs_group_last_backup_verify_state{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a",state="failed"} 1
# HELP pbs_group_info Owner and comment (group notes, otherwise the notes of the newest snapshot) of the backup group, always 1.
# TYPE pbs_group_info gauge
pbs_group_info{backup_id="100",backup_type="ct",comment="db",datastore="store1",namespace_root="",owner="pve@pbs!backup",pbs_namespace=""} 1
pbs_group_info{backup_id="100",backup_type="vm",comment="web",datastore="store1",namespace_root="",owner="pve@pbs!backup",pbs_namespace=""} 1
pbs_group_info{backup_id="benchmark",backup_type="host",comment="",datastore="store1",namespace_root="",owner="root@pam",pbs_namespace=""} 1
pbs_group_info{backup_id="200",backup_type="vm",comment="app server",datastore="store1",namespace_root="site-a",owner="site-a@pbs",pbs_namespace="site-a"} 1
`, "pbs_group_snapshots", "pbs_group_last_backup_timestamp_seconds", "pbs_group_last_backup_verify_state", "pbs_group_info")
}

// Without the snapshot list the group list still gives counts, timestamps and
// info; the collector reports the failure.
func TestGroupMetricsWithoutSnapshots(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=": {status: 500, body: `{"data":null,"message":"boom"}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"snapshots": false}})
	if n := testutil.CollectAndCount(e, "pbs_group_snapshots"); n != 4 {
		t.Errorf("pbs_group_snapshots: %d series, want 4", n)
	}
	// only the namespace with snapshots has verification states
	if n := testutil.CollectAndCount(e, "pbs_group_last_backup_verify_state"); n != 1 {
		t.Errorf("pbs_group_last_backup_verify_state: %d series, want 1", n)
	}
	compare(t, e, `
# HELP pbs_collector_success Whether a collector succeeded in the last scrape.
# TYPE pbs_collector_success gauge
pbs_collector_success{collector="datastores"} 1
pbs_collector_success{collector="groups"} 0
pbs_collector_success{collector="jobs"} 1
pbs_collector_success{collector="node"} 1
pbs_collector_success{collector="sync"} 1
pbs_collector_success{collector="tasks"} 1
pbs_collector_success{collector="time"} 1
pbs_collector_success{collector="version"} 1
pbs_collector_success{collector="zfs"} 1
`, "pbs_collector_success")
}

func TestNamespaceRoot(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a/b/c": "a"} {
		if got := namespaceRoot(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// A running backup is listed without manifest (no size, notes or
// verification); the newest finished snapshot is reported until it ends.
func TestGroupIgnoresRunningBackup(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=site-a": {body: `{"data":[
			{"backup-type":"vm","backup-id":"200","backup-time":1700000000,"comment":"app","size":4096,"verification":{"state":"ok"}},
			{"backup-type":"vm","backup-id":"200","backup-time":1700090000},
			{"backup-type":"vm","backup-id":"201","backup-time":1700090000}]}`},
		"/api2/json/admin/datastore/store1/groups?ns=site-a": {body: `{"data":[
			{"backup-type":"vm","backup-id":"200","backup-count":2,"last-backup":1700000000,"owner":"site-a@pbs"},
			{"backup-type":"vm","backup-id":"201","backup-count":1,"last-backup":1700090000,"owner":"site-a@pbs"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"snapshots": false}})
	got := seriesOf(t, e, "pbs_group_last_backup_verify_state", "pbs_group_info", "pbs_group_last_backup_timestamp_seconds")
	for _, want := range []string{
		`pbs_group_last_backup_verify_state{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a",state="ok"} 1`,
		`pbs_group_info{backup_id="200",backup_type="vm",comment="app",datastore="store1",namespace_root="site-a",owner="site-a@pbs",pbs_namespace="site-a"} 1`,
		// only a running first backup: no verification state yet
		`pbs_group_info{backup_id="201",backup_type="vm",comment="",datastore="store1",namespace_root="site-a",owner="site-a@pbs",pbs_namespace="site-a"} 1`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, s := range got {
		// PBS reports the running only snapshot as last-backup; it is no backup yet
		if strings.Contains(s, `backup_id="201"`) && !strings.HasPrefix(s, "pbs_group_info") {
			t.Errorf("series for a group with only a running backup: %s", s)
		}
		if strings.Contains(s, `backup_id="200"`) && strings.HasPrefix(s, "pbs_group_last_backup_timestamp_seconds") && !strings.HasSuffix(s, " 1.7e+09") {
			t.Errorf("the timestamp must be the one of the newest finished snapshot: %s", s)
		}
	}
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

// A group listed twice must not fail the scrape with duplicate series.
func TestDuplicateGroupIsSkipped(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/groups?ns=site-a": {body: `{"data":[
			{"backup-type":"vm","backup-id":"200","backup-count":1,"last-backup":1700000000,"owner":"site-a@pbs"},
			{"backup-type":"vm","backup-id":"200","backup-count":1,"last-backup":1700000000,"owner":"site-a@pbs"}]}`},
	})
	h := newMetricsHandler(map[string]*Module{"site": testModule(t, srv, ModuleConfig{})}, true, testTimeouts)
	if code, body := scrape(t, h, "module=site"); code != http.StatusOK || !strings.Contains(body, `pbs_collector_success{collector="groups"} 1`) {
		t.Errorf("status %d:\n%s", code, body)
	}
}

// Without the snapshot list, the group list's last-backup is only used where
// it is the newest finished snapshot (more than one snapshot), and no info
// series with an empty comment replaces the usual one.
func TestGroupsWithoutSnapshotList(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"snapshots": false}})
	got := seriesOf(t, e, "pbs_group_last_backup_timestamp_seconds", "pbs_group_info")
	want := []string{
		`pbs_group_info{backup_id="200",backup_type="vm",comment="app server",datastore="store1",namespace_root="site-a",owner="site-a@pbs",pbs_namespace="site-a"} 1`,
		`pbs_group_last_backup_timestamp_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1.7000864e+09`,
		`pbs_group_last_backup_timestamp_seconds{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1.7e+09`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
