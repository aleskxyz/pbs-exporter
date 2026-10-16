package main

import (
	"bytes"
	"fmt"
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
pbs_collector_success{collector="apt"} 1
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

func TestGroupExtendedMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_group_oldest_backup_timestamp_seconds Time of the oldest snapshot of the backup group.
# TYPE pbs_group_oldest_backup_timestamp_seconds gauge
pbs_group_oldest_backup_timestamp_seconds{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 1700000000
pbs_group_oldest_backup_timestamp_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1700000000
pbs_group_oldest_backup_timestamp_seconds{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1700000000
# HELP pbs_group_last_backup_size_bytes Size of the newest snapshot of the backup group.
# TYPE pbs_group_last_backup_size_bytes gauge
pbs_group_last_backup_size_bytes{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 512
pbs_group_last_backup_size_bytes{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 2048
pbs_group_last_backup_size_bytes{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 4096
# HELP pbs_group_snapshots_by_verify_state Number of snapshots of the backup group per verification state (ok, failed, none).
# TYPE pbs_group_snapshots_by_verify_state gauge
pbs_group_snapshots_by_verify_state{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace="",state="failed"} 0
pbs_group_snapshots_by_verify_state{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace="",state="none"} 0
pbs_group_snapshots_by_verify_state{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace="",state="ok"} 1
pbs_group_snapshots_by_verify_state{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace="",state="failed"} 0
pbs_group_snapshots_by_verify_state{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace="",state="none"} 1
pbs_group_snapshots_by_verify_state{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace="",state="ok"} 1
pbs_group_snapshots_by_verify_state{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a",state="failed"} 1
pbs_group_snapshots_by_verify_state{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a",state="none"} 0
pbs_group_snapshots_by_verify_state{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a",state="ok"} 0
# HELP pbs_group_last_verify_timestamp_seconds Start of the newest verification of any snapshot of the backup group (synced snapshots keep the verification of their source).
# TYPE pbs_group_last_verify_timestamp_seconds gauge
pbs_group_last_verify_timestamp_seconds{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 1700000256
pbs_group_last_verify_timestamp_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1700000256
pbs_group_last_verify_timestamp_seconds{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1700000256
# HELP pbs_group_last_backup_encrypted Whether the archives of the newest snapshot of the backup group are encrypted (signed only does not count).
# TYPE pbs_group_last_backup_encrypted gauge
pbs_group_last_backup_encrypted{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 0
pbs_group_last_backup_encrypted{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 0
pbs_group_last_backup_encrypted{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 0
# HELP pbs_group_last_backup_task_success Whether the newest finished backup task of the backup group ended OK.
# TYPE pbs_group_last_backup_task_success gauge
pbs_group_last_backup_task_success{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1
# HELP pbs_group_last_backup_task_end_timestamp_seconds End of the newest finished backup task of the backup group.
# TYPE pbs_group_last_backup_task_end_timestamp_seconds gauge
pbs_group_last_backup_task_end_timestamp_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1700086500
# HELP pbs_group_last_backup_duration_seconds Duration of the newest finished backup task of the backup group.
# TYPE pbs_group_last_backup_duration_seconds gauge
pbs_group_last_backup_duration_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 100
`, "pbs_group_oldest_backup_timestamp_seconds", "pbs_group_last_backup_size_bytes", "pbs_group_snapshots_by_verify_state",
		"pbs_group_last_verify_timestamp_seconds", "pbs_group_last_backup_encrypted", "pbs_group_backup_interval_seconds",
		"pbs_group_last_backup_task_success", "pbs_group_last_backup_task_end_timestamp_seconds", "pbs_group_last_backup_duration_seconds")
}

// Protected snapshots, encryption and the interval of a group with enough
// history.
func TestGroupHistory(t *testing.T) {
	const day = 86400
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/snapshots?ns=": {body: fmt.Sprintf(`{"data":[
			{"backup-type":"vm","backup-id":"100","backup-time":%d,"size":1,"protected":true},
			{"backup-type":"vm","backup-id":"100","backup-time":%d,"size":1},
			{"backup-type":"vm","backup-id":"100","backup-time":%d,"size":1},
			{"backup-type":"vm","backup-id":"100","backup-time":%d,"size":1,"fingerprint":"aa:bb","files":[
				{"filename":"index.json.blob","crypt-mode":"sign-only"},{"filename":"drive-scsi0.img.fidx","crypt-mode":"encrypt"}]}]}`,
			1700000000-14*day, 1700000000-7*day, 1700000000-day, 1700000000)},
	})
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_group_protected_snapshots Number of protected snapshots of the backup group.
# TYPE pbs_group_protected_snapshots gauge
pbs_group_protected_snapshots{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1
pbs_group_protected_snapshots{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 0
# HELP pbs_group_last_backup_encrypted Whether the archives of the newest snapshot of the backup group are encrypted (signed only does not count).
# TYPE pbs_group_last_backup_encrypted gauge
pbs_group_last_backup_encrypted{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1
pbs_group_last_backup_encrypted{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 0
# HELP pbs_group_backup_interval_seconds Typical time between snapshots of the backup group: the median of the gaps between its newest 4 snapshots; omitted below 3 snapshots.
# TYPE pbs_group_backup_interval_seconds gauge
pbs_group_backup_interval_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 518400
`, "pbs_group_protected_snapshots", "pbs_group_last_backup_encrypted", "pbs_group_backup_interval_seconds")
}

func TestBackupInterval(t *testing.T) {
	const day = 86400
	for name, tc := range map[string]struct {
		times []int64
		want  int64
	}{
		// keep-daily 7, keep-weekly 4: only the newest gaps count
		"thinned by retention": {[]int64{0, -day, -2 * day, -3 * day, -10 * day, -17 * day, -24 * day}, day},
		"unsorted":             {[]int64{-2 * day, 0, -day}, day},
		"two gaps":             {[]int64{0, -day, -4 * day}, 2 * day},
		"three gaps":           {[]int64{0, -7 * day, -8 * day, -15 * day}, 7 * day},
		"too few":              {[]int64{0, -day}, 0},
	} {
		g := &groupSummary{times: tc.times}
		got, ok := g.interval()
		if got != tc.want || ok != (tc.want != 0) {
			t.Errorf("%s: got %d, %t, want %d", name, got, ok, tc.want)
		}
	}
}

// Backup task worker IDs have no namespace: a task belongs to the group its
// user can write to (owner, or user of an owning token); a task that fits
// several groups, or a store with unknown groups, is left out.
func TestMatchBackupTasks(t *testing.T) {
	group := func(id, owner string) groupItem {
		return groupItem{BackupType: "vm", BackupID: id, Owner: owner}
	}
	all := []namespaceGroups{
		{store: "main", ns: "site-a", groups: []groupItem{group("230", "a@pbs"), group("231", "c@pbs"), group("232", "d@pbs!t1"), group("233", "x@pbs"), group("234", "y@pbs")}},
		{store: "main", ns: "site-b", groups: []groupItem{group("230", "b@pbs!tok"), group("231", "c@pbs"), group("232", "e@pbs")}},
		{store: "other", ns: "", groups: []groupItem{group("230", "a@pbs")}},
	}
	task := func(store, id, user string, start int64, status string) taskItem {
		return taskItem{WorkerType: "backup", WorkerID: store + ":vm/" + id, User: user, StartTime: start, EndTime: start + 10, Status: status}
	}
	tasks := []taskItem{
		task("main", "230", "a@pbs", 100, "OK"),
		task("main", "230", "a@pbs", 200, "backup failed"),
		task("main", "230", "b@pbs!tok", 250, "OK"),
		task("main", "230", "b@pbs", 300, "OK"),    // user of the owning token
		task("main", "231", "c@pbs", 100, "OK"),    // owner of both groups
		task("main", "232", "d@pbs!t2", 100, "OK"), // another token of the owner's user
		task("main", "233", "x@pbs", 100, "OK"),
		task("main", "233", "x@pbs", 500, ""),      // still running
		task("main", "234", "root@pam", 100, "OK"), // not the owner
		task("other", "230", "a@pbs", 50, "OK"),
		{WorkerType: "reader", WorkerID: "main:vm/233", User: "x@pbs", StartTime: 900, EndTime: 910, Status: "OK"},
	}
	want := map[groupRef]int64{
		{"main", "site-a", "vm", "230"}: 200,
		{"main", "site-b", "vm", "230"}: 300,
		{"main", "site-a", "vm", "233"}: 100,
		{"other", "", "vm", "230"}:      50,
	}
	got := matchBackupTasks(all, tasks, nil)
	if len(got) != len(want) {
		t.Errorf("matched %d groups, want %d: %v", len(got), len(want), got)
	}
	for ref, start := range want {
		if got[ref].StartTime != start {
			t.Errorf("%v: matched task started at %d, want %d", ref, got[ref].StartTime, start)
		}
	}

	// the groups of a namespace of "main" are unknown: no matches in "main"
	got = matchBackupTasks(all, tasks, map[string]bool{"main": true})
	if len(got) != 1 || got[groupRef{"other", "", "vm", "230"}].StartTime != 50 {
		t.Errorf("matched %v with an incomplete store", got)
	}
}

func TestCanWrite(t *testing.T) {
	for _, tc := range []struct {
		authID, owner string
		want          bool
	}{
		{"a@pbs", "a@pbs", true},
		{"a@pbs!tok", "a@pbs!tok", true},
		{"a@pbs", "a@pbs!tok", true},
		{"a@pbs!tok", "a@pbs", false},
		{"a@pbs!t2", "a@pbs!t1", false},
		{"root@pam", "a@pbs", false},
	} {
		if got := canWrite(tc.authID, tc.owner); got != tc.want {
			t.Errorf("canWrite(%q, %q) = %t, want %t", tc.authID, tc.owner, got, tc.want)
		}
	}
}

func TestParseBackupWorkerID(t *testing.T) {
	if got, ok := parseBackupWorkerID("PBS-3309-ENF-DC01:vm/1560"); !ok || got != (backupTaskKey{"PBS-3309-ENF-DC01", "vm", "1560"}) {
		t.Errorf("got %+v, %t", got, ok)
	}
	for _, in := range []string{"", "store", "store:vm", "store:/1", ":vm/1"} {
		if _, ok := parseBackupWorkerID(in); ok {
			t.Errorf("%q: expected no match", in)
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

// A key fingerprint is also set for backups that are only signed; encrypted
// are snapshots whose archives are all encrypted.
func TestSnapshotEncrypted(t *testing.T) {
	snap := func(modes ...string) snapshotItem {
		var s snapshotItem
		s.Files = append(s.Files, struct {
			Filename  string `json:"filename"`
			CryptMode string `json:"crypt-mode"`
		}{"index.json.blob", "sign-only"})
		for i, m := range modes {
			s.Files = append(s.Files, struct {
				Filename  string `json:"filename"`
				CryptMode string `json:"crypt-mode"`
			}{fmt.Sprintf("a%d.fidx", i), m})
		}
		return s
	}
	for name, tc := range map[string]struct {
		s    snapshotItem
		want bool
	}{
		"encrypted": {snap("encrypt", "encrypt"), true},
		// the client log uploaded after the backup is not in the manifest
		"with client log": {func() snapshotItem {
			sn := snap("encrypt", "encrypt")
			sn.Files = append(sn.Files, struct {
				Filename  string `json:"filename"`
				CryptMode string `json:"crypt-mode"`
			}{"client.log.blob", ""})
			return sn
		}(), true},
		"no crypt modes": {snap(""), false},
		"sign-only":      {snap("sign-only"), false},
		"plain":          {snap("none"), false},
		"mixed":          {snap("encrypt", "none"), false},
		"only index":     {snap(), false},
		"running":        {snapshotItem{}, false},
	} {
		if got := tc.s.encrypted(); got != tc.want {
			t.Errorf("%s: got %t, want %t", name, got, tc.want)
		}
	}
}

// A group listed twice keeps its backup task metrics, the duration of a task
// can not be negative, and a run with warnings is successful.
func TestBackupTaskMetricsEdgeCases(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/datastore/store1/groups?ns=site-a": {body: `{"data":[
			{"backup-type":"vm","backup-id":"200","backup-count":1,"last-backup":1700000000,"owner":"site-a@pbs"},
			{"backup-type":"vm","backup-id":"200","backup-count":1,"last-backup":1700000000,"owner":"site-a@pbs"}]}`},
		"/api2/json/nodes/localhost/tasks": {body: `{"data":[
			{"worker_type":"backup","worker_id":"store1:vm/200","user":"site-a@pbs","starttime":1700000100,"endtime":1700000000,"status":"WARNINGS: 1"}]}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_group_last_backup_task_success", "pbs_group_last_backup_duration_seconds")
	want := []string{
		`pbs_group_last_backup_duration_seconds{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 0`,
		`pbs_group_last_backup_task_success{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Without Datastore.Audit on the whole datastore, PBS leaves out the
// namespaces the token can not see; a backup task could belong to a group in
// one of them, so tasks are not matched.
func TestBackupTasksWithHiddenNamespaces(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {body: `{"data":{"/datastore":{"Datastore.Audit":false},"/remote":{"Remote.Audit":true},"/system":{"Sys.Audit":true}}}`},
	})
	if got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_group_last_backup_task_success"); len(got) != 0 {
		t.Errorf("tasks matched without access to all namespaces: %v", got)
	}
	p := permissions{"/datastore/store1": {"Datastore.Audit": true}}
	if !p.canAuditDatastore("store1") || p.canAuditDatastore("store2") {
		t.Error("canAuditDatastore")
	}
}
