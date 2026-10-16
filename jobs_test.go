package main

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestParseUPIDStart(t *testing.T) {
	for upid, want := range map[string]int64{
		// from a real task, the worker ID escapes ":" and "-"
		`UPID:Server3309-PBS-ENF:0000139F:000008BF:00001DA6:6AD10890:prune:PBS\x2d3309\x2dENF\x2dDC01\x3a\x3avm-1560:root@pam:`: 1792084112,
		`UPID:pbs:00000001:00000002:00000003:6553F100:verificationjob:store1\x3av\x2d1:root@pam:`:                               1700000000,
		``:                               0,
		`UPID:pbs:1:2:3`:                 0,
		`UPID:pbs:1:2:3:zz:a:b:c:`:       0,
		`NOPE:pbs:1:2:3:6553F100:a:b:c:`: 0,
	} {
		got, ok := parseUPIDStart(upid)
		if got != want || ok != (want != 0) {
			t.Errorf("%q: got %d, %t, want %d", upid, got, ok, want)
		}
	}
}

func TestParseSyncWorkerID(t *testing.T) {
	got, ok := parseSyncWorkerID("PBS_DC1:PBS-3309-ENF-DC01:main:DC1:s-431d459b-634d")
	if want := (syncWorkerID{"PBS_DC1", "PBS-3309-ENF-DC01", "main", "DC1", "s-431d459b-634d"}); !ok || got != want {
		t.Errorf("got %+v, %t", got, ok)
	}
	// root namespace
	if got, ok := parseSyncWorkerID("r:rs:store::s-1"); !ok || got.ns != "" || got.jobID != "s-1" {
		t.Errorf("got %+v, %t", got, ok)
	}
	for _, in := range []string{"", "a:b:c:d", "a:b:c:d:"} {
		if _, ok := parseSyncWorkerID(in); ok {
			t.Errorf("%q: expected no match", in)
		}
	}
}

// A job that never ran has only its next run; a failed run reports 0; GC
// takes its duration from PBS.
func TestJobMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	compare(t, e, `
# HELP pbs_job_last_run_success Whether the last run of the job ended without error (OK or with warnings).
# TYPE pbs_job_last_run_success gauge
pbs_job_last_run_success{datastore="store1",job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config"} 0
pbs_job_last_run_success{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config"} 1
pbs_job_last_run_success{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1
pbs_job_last_run_success{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 1
# HELP pbs_job_last_run_end_timestamp_seconds End of the last run of the job.
# TYPE pbs_job_last_run_end_timestamp_seconds gauge
pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config"} 1700000060
pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config"} 1700000300
pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1700001000
pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 1700003600
# HELP pbs_job_next_run_timestamp_seconds Next scheduled run of the job.
# TYPE pbs_job_next_run_timestamp_seconds gauge
pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config"} 1700086400
pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config"} 1700003600
pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1700604800
pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 1700604800
pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="v-2",job_type="verify",pbs_namespace="site-a",source="config"} 1700100000
# HELP pbs_job_interval_seconds Time between the start of the last run and the next run of the job.
# TYPE pbs_job_interval_seconds gauge
pbs_job_interval_seconds{datastore="store1",job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config"} 86400
pbs_job_interval_seconds{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config"} 3600
pbs_job_interval_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 604800
pbs_job_interval_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 604800
# HELP pbs_job_last_run_duration_seconds Duration of the last run of the job.
# TYPE pbs_job_last_run_duration_seconds gauge
pbs_job_last_run_duration_seconds{datastore="store1",job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config"} 60
pbs_job_last_run_duration_seconds{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config"} 300
pbs_job_last_run_duration_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1000
pbs_job_last_run_duration_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 3600
# HELP pbs_verify_job_info Settings of a verify job, always 1.
# TYPE pbs_verify_job_info gauge
pbs_verify_job_info{datastore="store1",ignore_verified="true",job_id="v-1",max_depth="",outdated_after="30",pbs_namespace="",schedule="sat 02:00"} 1
pbs_verify_job_info{datastore="store1",ignore_verified="",job_id="v-2",max_depth="",outdated_after="",pbs_namespace="site-a",schedule="daily"} 1
# HELP pbs_prune_job_keep Retention setting of a prune job.
# TYPE pbs_prune_job_keep gauge
pbs_prune_job_keep{datastore="store1",job_id="p-1",keep="daily",pbs_namespace="site-a"} 7
pbs_prune_job_keep{datastore="store1",job_id="p-1",keep="last",pbs_namespace="site-a"} 3
# HELP pbs_sync_job_info Settings of a sync job, always 1. With source="tasks" only datastore, pbs_namespace, remote and remote_store are known.
# TYPE pbs_sync_job_info gauge
pbs_sync_job_info{datastore="store1",group_filter="type:vm,regex:^1",job_id="s-1",pbs_namespace="site-a",remote="pbs-b",remote_ns="site-a",remote_store="main",remove_vanished="true",schedule="hourly",source="config",sync_direction="pull",transfer_last="2"} 1
# HELP pbs_remote_info A remote PBS that sync jobs read from or write to, always 1.
# TYPE pbs_remote_info gauge
pbs_remote_info{host="pbs-b.example.org",remote="pbs-b"} 1
# HELP pbs_tasks_running Number of running tasks.
# TYPE pbs_tasks_running gauge
pbs_tasks_running{worker_type="backup"} 2
pbs_tasks_running{worker_type="syncjob"} 1
`, "pbs_job_last_run_success", "pbs_job_last_run_end_timestamp_seconds", "pbs_job_next_run_timestamp_seconds",
		"pbs_job_interval_seconds", "pbs_job_last_run_duration_seconds", "pbs_verify_job_info", "pbs_prune_job_keep",
		"pbs_sync_job_info", "pbs_remote_info", "pbs_tasks_running")
}

// Without Remote.Audit, PBS lists no sync jobs and no error. The collector
// reports the missing privilege and takes the last run of each sync job from
// the task log (finished tasks only).
func TestSyncJobsWithoutRemoteAudit(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":            {body: `{"data":{"/":{"Datastore.Audit":true},"/remote":{"Datastore.Audit":true,"Sys.Audit":true},"/system":{"Sys.Audit":true}}}`},
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[]}`},
		"/api2/json/config/remote":                 {body: `{"data":[]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	compare(t, e, `
# HELP pbs_job_last_run_success Whether the last run of the job ended without error (OK or with warnings).
# TYPE pbs_job_last_run_success gauge
pbs_job_last_run_success{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="tasks"} 0
pbs_job_last_run_success{datastore="store1",job_id="s-2",job_type="sync",pbs_namespace="",source="tasks"} 1
# HELP pbs_job_last_run_end_timestamp_seconds End of the last run of the job.
# TYPE pbs_job_last_run_end_timestamp_seconds gauge
pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="tasks"} 1700110100
pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="s-2",job_type="sync",pbs_namespace="",source="tasks"} 1700001060
# HELP pbs_job_last_run_duration_seconds Duration of the last run of the job.
# TYPE pbs_job_last_run_duration_seconds gauge
pbs_job_last_run_duration_seconds{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="tasks"} 100
pbs_job_last_run_duration_seconds{datastore="store1",job_id="s-2",job_type="sync",pbs_namespace="",source="tasks"} 60
# HELP pbs_sync_job_info Settings of a sync job, always 1. With source="tasks" only datastore, pbs_namespace, remote and remote_store are known.
# TYPE pbs_sync_job_info gauge
pbs_sync_job_info{datastore="store1",group_filter="",job_id="s-1",pbs_namespace="site-a",remote="pbs-b",remote_ns="",remote_store="main",remove_vanished="",schedule="",source="tasks",sync_direction="",transfer_last=""} 1
pbs_sync_job_info{datastore="store1",group_filter="",job_id="s-2",pbs_namespace="",remote="pbs-c",remote_ns="",remote_store="other",remove_vanished="",schedule="",source="tasks",sync_direction="",transfer_last=""} 1
# HELP pbs_collector_success Whether a collector succeeded in the last scrape.
# TYPE pbs_collector_success gauge
pbs_collector_success{collector="apt"} 1
pbs_collector_success{collector="datastores"} 1
pbs_collector_success{collector="groups"} 1
pbs_collector_success{collector="node"} 1
pbs_collector_success{collector="snapshots"} 1
pbs_collector_success{collector="sync"} 0
pbs_collector_success{collector="tasks"} 1
pbs_collector_success{collector="time"} 1
pbs_collector_success{collector="version"} 1
pbs_collector_success{collector="zfs"} 1
`, "pbs_job_last_run_success", "pbs_job_last_run_end_timestamp_seconds", "pbs_job_next_run_timestamp_seconds",
		"pbs_job_last_run_duration_seconds", "pbs_sync_job_info", "pbs_remote_info", "pbs_collector_success")
	if !strings.Contains(logs.String(), "missing privilege Remote.Audit on /remote") {
		t.Errorf("the log does not name the missing privilege:\n%s", logs)
	}
}

func TestHasRemoteAudit(t *testing.T) {
	for name, tc := range map[string]struct {
		perms permissions
		want  bool
	}{
		"propagating":     {permissions{"/remote": {"Remote.Audit": true}}, true},
		"not propagating": {permissions{"/remote": {"Remote.Audit": false}}, false},
		"one remote only": {permissions{"/remote": {"Sys.Audit": true}, "/remote/pbs-b": {"Remote.Audit": true}}, false},
		"similar path":    {permissions{"/remotely-other": {"Remote.Audit": true}}, false},
		"none":            {permissions{}, false},
	} {
		if got := tc.perms.hasRemoteAudit(); got != tc.want {
			t.Errorf("%s: got %t, want %t", name, got, tc.want)
		}
	}
	if p := (permissions{"/remote/pbs-b": {"Remote.Audit": false}}); !p.canAuditRemote("pbs-b") || p.canAuditRemote("pbs-c") {
		t.Error("canAuditRemote")
	}
}

// The task log is read with since and limit, once per scrape, and a full
// result is reported.
func TestTaskLogQuery(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":                      {body: `{"data":{}}`},
		"/api2/json/nodes/localhost/tasks?limit=2&running=1": {body: `{"data":[]}`},
	})
	start := time.Now()
	e := testExporter(t, srv, ModuleConfig{TaskLookback: ptr(Duration(48 * time.Hour)), TaskLimit: ptr(2)})
	if n := testutil.CollectAndCount(e, "pbs_job_last_run_success"); n == 0 {
		t.Fatal("no sync jobs from the task log")
	}
	var reads int
	for _, r := range srv.Requests() {
		path, query, _ := strings.Cut(r, "?")
		if path != "/api2/json/nodes/localhost/tasks" || strings.Contains(query, "running") {
			continue
		}
		reads++
		q, _ := url.ParseQuery(query)
		since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
		if want := start.Add(-48 * time.Hour).Unix(); q.Get("limit") != "2" || since < want-5 || since > want+5 {
			t.Errorf("task log query %q, want limit=2 and since about %d", query, want)
		}
	}
	if reads != 1 {
		t.Errorf("task log read %d times, want once", reads)
	}
	if !strings.Contains(logs.String(), "more tasks than task_limit") {
		t.Errorf("no warning about the task limit:\n%s", logs)
	}
}

// With Remote.Audit on one remote only, PBS lists only that remote's jobs;
// the others come from the task log and the collector reports the gap.
func TestSyncJobsWithPartialRemoteAudit(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {body: `{"data":{"/remote":{"Sys.Audit":true},"/remote/pbs-b":{"Remote.Audit":true},"/system":{"Sys.Audit":true}}}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	got := seriesOf(t, e, "pbs_job_last_run_success", "pbs_collector_success")
	for _, want := range []string{
		`pbs_job_last_run_success{datastore="store1",job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config"} 1`,
		`pbs_job_last_run_success{datastore="store1",job_id="s-2",job_type="sync",pbs_namespace="",source="tasks"} 1`,
		`pbs_collector_success{collector="sync"} 0`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, strings.Join(got, "\n"))
		}
	}
	for _, line := range got {
		if strings.Contains(line, `job_id="s-1"`) && strings.Contains(line, `source="tasks"`) {
			t.Errorf("a listed job is also taken from the task log: %s", line)
		}
	}
}

// Without Remote.Audit but also without sync jobs of remotes, nothing is
// missing: the collector succeeds.
func TestSyncWithoutRemoteJobs(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":            {body: `{"data":{"/remote":{"Sys.Audit":true},"/system":{"Sys.Audit":true}}}`},
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[]}`},
		"/api2/json/config/remote":                 {body: `{"data":[]}`},
		"/api2/json/nodes/localhost/tasks":         {body: `{"data":[]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="sync"} 1`) {
		t.Errorf("sync collector failed:\n%s", strings.Join(got, "\n"))
	}
}

// While a job runs, PBS reports the running task without state and end; the
// previous run comes from the task log.
func TestRunningJobUsesPreviousRun(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/verify": {body: `{"data":[{"id":"v-1","store":"store1","ns":"","schedule":"sat 02:00","next-run":1700604800,
			"last-run-upid":"UPID:pbs:00000001:00000002:00000009:6553F100:verificationjob:store1\\x3av\\x2d1:root@pam:"}]}`},
		"/api2/json/admin/gc": {body: `{"data":[{"store":"store1","next-run":1700604800,
			"upid":"UPID:pbs:00000001:00000002:00000008:6553F100:garbage_collection:store1:root@pam:"}]}`},
		"/api2/json/nodes/localhost/tasks": {body: `{"data":[
			{"upid":"x","worker_type":"verificationjob","worker_id":"store1:v-1","user":"root@pam","starttime":1700000000},
			{"upid":"x","worker_type":"verificationjob","worker_id":"store1:v-1","user":"root@pam","starttime":1699395200,"endtime":1699398800,"status":"WARNINGS: 1"},
			{"upid":"x","worker_type":"verificationjob","worker_id":"store1:v-1","user":"root@pam","starttime":1698790400,"endtime":1698791400,"status":"OK"},
			{"upid":"x","worker_type":"verificationjob","worker_id":"store1:v-2","user":"root@pam","starttime":1699999000,"endtime":1699999500,"status":"OK"},
			{"upid":"x","worker_type":"garbage_collection","worker_id":"store1","user":"root@pam","starttime":1699395200,"endtime":1699396200,"status":"OK"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"sync": false}})
	got := seriesOf(t, e, "pbs_job_last_run_success", "pbs_job_last_run_end_timestamp_seconds", "pbs_job_last_run_duration_seconds", "pbs_job_interval_seconds")
	for _, want := range []string{
		`pbs_job_last_run_success{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 1`,
		`pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 1.6993988e+09`,
		`pbs_job_last_run_duration_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 3600`,
		`pbs_job_interval_seconds{datastore="store1",job_id="v-1",job_type="verify",pbs_namespace="",source="config"} 604800`,
		`pbs_job_last_run_success{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1`,
		`pbs_job_last_run_duration_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1000`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, strings.Join(got, "\n"))
		}
	}
}

func TestParseUPIDWorker(t *testing.T) {
	for upid, want := range map[string][2]string{
		`UPID:Server3309-PBS-ENF:0000139F:000008BF:00001DA6:6AD10890:prune:PBS\x2d3309\x2dENF\x2dDC01\x3a\x3avm-1560:root@pam:`:                           {"prune", "PBS-3309-ENF-DC01::vm/1560"},
		`UPID:pbs-01:0001ECCC:007E92D3:00000390:6ACA9940:syncjob:PBS_DC1\x3aPBS\x2d3309\x2dENF\x2dDC01\x3amain\x3aDC1\x3as\x2d431d459b\x2d634d:root@pam:`: {"syncjob", "PBS_DC1:PBS-3309-ENF-DC01:main:DC1:s-431d459b-634d"},
		`UPID:pbs:1:2:3:6553F100:verificationjob:store1\x3av\x2d1:root@pam:`:                                                                              {"verificationjob", "store1:v-1"},
	} {
		typ, id, ok := parseUPIDWorker(upid)
		if !ok || typ != want[0] || id != want[1] {
			t.Errorf("%s: got %q %q %t", upid, typ, id, ok)
		}
	}
	if _, _, ok := parseUPIDWorker("UPID:short"); ok {
		t.Error("expected no match")
	}
	if got, _ := parseSyncWorkerID("-:store2:store1::s-local"); got.remote != "" || got.remoteStore != "store2" {
		t.Errorf("local sync job: %+v", got)
	}
}

// GC: PBS's UPID and duration are the ones of the last successful run. While
// a GC runs, the previous run comes from the task log and there is no
// interval; after a failed run, its start comes from the task log.
func TestGCRuns(t *testing.T) {
	successful := `"upid":"UPID:pbs:00000001:00000002:00000004:6553F100:garbage_collection:store1:root@pam:","duration":1000`
	tasks := `{"data":[
		{"worker_type":"garbage_collection","worker_id":"store1","user":"root@pam","starttime":1700000000,"endtime":1700001000,"status":"OK"},
		{"worker_type":"garbage_collection","worker_id":"store1","user":"root@pam","starttime":1700600000,"endtime":1700600500,"status":"disk full"},
		{"worker_type":"garbage_collection","worker_id":"store1","user":"root@pam","starttime":1701200000}]}`
	for name, tc := range map[string]struct {
		gc   string
		want []string
	}{
		"running": {`{"store":"store1","next-run":1701300000,` + successful + `}`, []string{
			`pbs_job_last_run_duration_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 500`,
			`pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1.7006005e+09`,
			`pbs_job_last_run_success{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 0`,
			`pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1.7013e+09`,
		}},
		"failed": {`{"store":"store1","last-run-state":"disk full","last-run-endtime":1700600500,"next-run":1701200000,` + successful + `}`, []string{
			`pbs_job_interval_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 600000`,
			`pbs_job_last_run_duration_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 500`,
			`pbs_job_last_run_end_timestamp_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1.7006005e+09`,
			`pbs_job_last_run_success{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 0`,
			`pbs_job_next_run_timestamp_seconds{datastore="store1",job_id="store1",job_type="gc",pbs_namespace="",source="config"} 1.7012e+09`,
		}},
	} {
		srv := newFakePBS(t, map[string]fakeResp{
			"/api2/json/admin/gc":              {body: `{"data":[` + tc.gc + `]}`},
			"/api2/json/admin/verify":          {body: `{"data":[]}`},
			"/api2/json/admin/prune":           {body: `{"data":[]}`},
			"/api2/json/nodes/localhost/tasks": {body: tasks},
		})
		got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"sync": false}}),
			"pbs_job_last_run_success", "pbs_job_last_run_end_timestamp_seconds", "pbs_job_next_run_timestamp_seconds",
			"pbs_job_interval_seconds", "pbs_job_last_run_duration_seconds")
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}
}

// Prune job tasks are named after the datastore and namespace: while one of
// two jobs on the same namespace runs, its previous run is not known.
func TestRunningPruneJobsSharingANamespace(t *testing.T) {
	running := `"last-run-upid":"UPID:pbs:00000001:00000002:00000009:6553F100:prunejob:store1:root@pam:","next-run":1700086400`
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/prune": {body: `{"data":[
			{"id":"p-1","store":"store1","last-run-state":"OK","last-run-endtime":1699990000,"last-run-upid":"UPID:pbs:00000001:00000002:00000008:6553CAF0:prunejob:store1:root@pam:","next-run":1700086400},
			{"id":"p-2","store":"store1",` + running + `},
			{"id":"p-3","store":"store2",` + strings.ReplaceAll(running, "store1", "store2") + `}]}`},
		"/api2/json/admin/verify": {body: `{"data":[]}`},
		"/api2/json/admin/gc":     {body: `{"data":[]}`},
		"/api2/json/nodes/localhost/tasks": {body: `{"data":[
			{"worker_type":"prunejob","worker_id":"store1","user":"root@pam","starttime":1699990000,"endtime":1699990010,"status":"OK"},
			{"worker_type":"prunejob","worker_id":"store2","user":"root@pam","starttime":1699990000,"endtime":1699990020,"status":"OK"}]}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"sync": false}}), "pbs_job_last_run_success")
	want := []string{
		`pbs_job_last_run_success{datastore="store1",job_id="p-1",job_type="prune",pbs_namespace="",source="config"} 1`,
		`pbs_job_last_run_success{datastore="store2",job_id="p-3",job_type="prune",pbs_namespace="",source="config"} 1`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A sync job missing from the list is only taken from the task log when its
// remote can not be audited; otherwise, and for local jobs, it was deleted.
func TestDeletedSyncJobsAreNotHidden(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":            {body: `{"data":{"/remote/pbs-b":{"Remote.Audit":true},"/system":{"Sys.Audit":true}}}`},
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[]}`},
		"/api2/json/nodes/localhost/tasks": {body: `{"data":[
			{"worker_type":"syncjob","worker_id":"pbs-b:main:store1:site-a:s-1","user":"root@pam","starttime":1700000000,"endtime":1700000300,"status":"OK"},
			{"worker_type":"syncjob","worker_id":"-:store2:store1::s-local","user":"root@pam","starttime":1700000000,"endtime":1700000300,"status":"OK"}]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	if got := seriesOf(t, e, "pbs_job_last_run_success"); len(got) != 0 {
		t.Errorf("deleted jobs are reported: %v", got)
	}
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="sync"} 1`) {
		t.Errorf("sync collector failed:\n%s", strings.Join(got, "\n"))
	}
}

// Without Sys.Audit on /system/tasks, PBS lists only the token's own tasks
// without an error; the collectors that use the task log report it.
func TestFilteredTaskLog(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {body: `{"data":{"/":{"Datastore.Audit":true},"/remote":{"Remote.Audit":true}}}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_collector_success")
	if !slices.Contains(got, `pbs_collector_success{collector="tasks"} 0`) {
		t.Errorf("tasks collector does not report the filtered task log:\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(logs.String(), "missing privilege Sys.Audit on /system/tasks") {
		t.Errorf("no log about the missing privilege:\n%s", logs)
	}
}

// When the permissions can not be read, the sync jobs are still exported.
func TestSyncWithoutPermissions(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	if got := seriesOf(t, e, "pbs_sync_job_info", "pbs_remote_info"); len(got) != 2 {
		t.Errorf("got %v, want the sync job and the remote", got)
	}
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
