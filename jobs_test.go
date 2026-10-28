package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"
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

// The run metrics are the job state PBS reports: a job that never ran has
// only its next run, a failed run reports 0, and GC takes its start from the
// UPID of its last successful run.
func TestJobMetrics(t *testing.T) {
	srv := newFakePBS(t, nil)
	e := testExporter(t, srv, ModuleConfig{})
	got := seriesOf(t, e, "pbs_job_last_run_success", "pbs_job_last_run_start_timestamp_seconds", "pbs_job_last_run_end_timestamp_seconds",
		"pbs_job_next_run_timestamp_seconds")
	want := []string{
		`pbs_job_last_run_end_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="store1",job_type="gc"} 1.700001e+09`,
		`pbs_job_last_run_end_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="v-1",job_type="verify"} 1.7000036e+09`,
		`pbs_job_last_run_end_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="p-1",job_type="prune"} 1.70000006e+09`,
		`pbs_job_last_run_end_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="s-1",job_type="sync"} 1.7000003e+09`,
		`pbs_job_last_run_start_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="store1",job_type="gc"} 1.7e+09`,
		`pbs_job_last_run_start_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="v-1",job_type="verify"} 1.7e+09`,
		`pbs_job_last_run_start_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="p-1",job_type="prune"} 1.7e+09`,
		`pbs_job_last_run_start_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="s-1",job_type="sync"} 1.7e+09`,
		`pbs_job_last_run_success{datastore="store1",datastore_namespace="",job_id="store1",job_type="gc"} 1`,
		`pbs_job_last_run_success{datastore="store1",datastore_namespace="",job_id="v-1",job_type="verify"} 1`,
		`pbs_job_last_run_success{datastore="store1",datastore_namespace="site-a",job_id="p-1",job_type="prune"} 0`,
		`pbs_job_last_run_success{datastore="store1",datastore_namespace="site-a",job_id="s-1",job_type="sync"} 1`,
		`pbs_job_next_run_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="store1",job_type="gc"} 1.7006048e+09`,
		`pbs_job_next_run_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="v-1",job_type="verify"} 1.7006048e+09`,
		`pbs_job_next_run_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="p-1",job_type="prune"} 1.7000864e+09`,
		`pbs_job_next_run_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="s-1",job_type="sync"} 1.7000036e+09`,
		`pbs_job_next_run_timestamp_seconds{datastore="store1",datastore_namespace="site-a",job_id="v-2",job_type="verify"} 1.7001e+09`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	compare(t, e, `
# HELP pbs_verify_job_info Settings of a verify job, always 1.
# TYPE pbs_verify_job_info gauge
pbs_verify_job_info{datastore="store1",datastore_namespace="",ignore_verified="true",job_id="v-1",max_depth="",outdated_after="30",schedule="sat 02:00"} 1
pbs_verify_job_info{datastore="store1",datastore_namespace="site-a",ignore_verified="",job_id="v-2",max_depth="",outdated_after="",schedule="daily"} 1
# HELP pbs_prune_job_info Settings of a prune job, always 1.
# TYPE pbs_prune_job_info gauge
pbs_prune_job_info{datastore="store1",datastore_namespace="site-a",disable="",job_id="p-1",max_depth="",schedule="daily"} 1
# HELP pbs_prune_job_keep Retention setting of a prune job.
# TYPE pbs_prune_job_keep gauge
pbs_prune_job_keep{datastore="store1",datastore_namespace="site-a",job_id="p-1",keep="daily"} 7
pbs_prune_job_keep{datastore="store1",datastore_namespace="site-a",job_id="p-1",keep="last"} 3
# HELP pbs_sync_job_info Settings of a sync job, always 1. datastore and datastore_namespace are local, remote_* the other side; sync_direction (empty when PBS reports none, which PBS treats as pull) tells which side is the source.
# TYPE pbs_sync_job_info gauge
pbs_sync_job_info{datastore="store1",datastore_namespace="site-a",encrypted_only="",group_filter="regex:^1,type:vm",job_id="s-1",max_depth="",remote="pbs-b",remote_ns="site-a",remote_store="main",remove_vanished="true",schedule="hourly",sync_direction="pull",transfer_last="2",verified_only=""} 1
# HELP pbs_remote_info A remote PBS that sync jobs read from or write to, always 1. fingerprint is the pinned certificate fingerprint (lower case), empty when the remote's certificate is trusted otherwise.
# TYPE pbs_remote_info gauge
pbs_remote_info{auth_id="sync@pbs",fingerprint="aa:bb:cc",host="pbs-b.example.org",port="8007",remote="pbs-b"} 1
# HELP pbs_tasks_running Number of running tasks.
# TYPE pbs_tasks_running gauge
pbs_tasks_running{worker_type="backup"} 2
pbs_tasks_running{worker_type="syncjob"} 1
`, "pbs_verify_job_info", "pbs_prune_job_info", "pbs_prune_job_keep", "pbs_sync_job_info", "pbs_remote_info", "pbs_tasks_running")
}

// While a job runs, PBS reports the running task as last run, without state
// and end.
func TestRunningJob(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/verify": {body: `{"data":[{"id":"v-1","store":"store1","ns":"","schedule":"sat 02:00","next-run":1700604800,
			"last-run-upid":"UPID:pbs:00000001:00000002:00000009:6553F100:verificationjob:store1\\x3av\\x2d1:root@pam:"}]}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"sync": false}}),
		"pbs_job_last_run_success", "pbs_job_last_run_start_timestamp_seconds", "pbs_job_last_run_end_timestamp_seconds")
	want := `pbs_job_last_run_start_timestamp_seconds{datastore="store1",datastore_namespace="",job_id="v-1",job_type="verify"} 1.7e+09`
	if !slices.Contains(got, want) || slices.ContainsFunc(got, func(l string) bool { return strings.Contains(l, `job_id="v-1"`) && l != want }) {
		t.Errorf("got\n%s\nwant only the start of v-1:\n%s", strings.Join(got, "\n"), want)
	}
}

// Without Remote.Audit, PBS lists no sync jobs and no error. The collector
// reports the missing privilege.
func TestSyncJobsWithoutRemoteAudit(t *testing.T) {
	logs := captureLogs(t)
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":            {body: `{"data":{"/":{"Datastore.Audit":true},"/datastore":{"Datastore.Audit":true},"/remote":{"Datastore.Audit":true,"Sys.Audit":true},"/system":{"Sys.Audit":true}}}`},
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[]}`},
		"/api2/json/config/remote":                 {body: `{"data":[]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	got := seriesOf(t, e, "pbs_job_last_run_success", "pbs_sync_job_info", "pbs_collector_success")
	if slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, "pbs_job_") || strings.HasPrefix(l, "pbs_sync_job_") }) {
		t.Errorf("hidden jobs are exported:\n%s", strings.Join(got, "\n"))
	}
	if !slices.Contains(got, `pbs_collector_success{collector="sync"} 0`) {
		t.Errorf("sync collector does not fail:\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(logs.String(), "missing privilege Remote.Audit on /remote") {
		t.Errorf("the log does not name the missing privilege:\n%s", logs)
	}
}

// With Remote.Audit on one remote only, PBS lists only that remote's jobs;
// they are exported, and the collector reports that the list may be
// incomplete.
func TestSyncJobsWithPartialRemoteAudit(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {body: `{"data":{"/datastore":{"Datastore.Audit":true},"/remote":{"Sys.Audit":true},"/remote/pbs-b":{"Remote.Audit":true},"/system":{"Sys.Audit":true}}}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	got := seriesOf(t, e, "pbs_job_last_run_success", "pbs_collector_success")
	for _, want := range []string{
		`pbs_job_last_run_success{datastore="store1",datastore_namespace="site-a",job_id="s-1",job_type="sync"} 1`,
		`pbs_collector_success{collector="sync"} 0`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, strings.Join(got, "\n"))
		}
	}
}

// With Remote.Audit on /remote, the list is complete: the collector
// succeeds, also without sync jobs.
func TestSyncWithRemoteAudit(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions":            {body: `{"data":{"/datastore":{"Datastore.Audit":true},"/remote":{"Remote.Audit":true},"/system":{"Sys.Audit":true}}}`},
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[]}`},
		"/api2/json/config/remote":                 {body: `{"data":[]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	if got := seriesOf(t, e, "pbs_collector_success"); !slices.Contains(got, `pbs_collector_success{collector="sync"} 1`) {
		t.Errorf("sync collector failed:\n%s", strings.Join(got, "\n"))
	}
}

// GC: PBS's UPID is the one of the last successful run, so it is the start
// of the last run only when that succeeded; a running GC has no state.
func TestGCRuns(t *testing.T) {
	successful := `"upid":"UPID:pbs:00000001:00000002:00000004:6553F100:garbage_collection:store1:root@pam:"`
	labels := `{datastore="store1",datastore_namespace="",job_id="store1",job_type="gc"}`
	for name, tc := range map[string]struct {
		gc   string
		want []string
	}{
		"running": {`{"store":"store1","next-run":1701300000,` + successful + `}`, []string{
			`pbs_job_next_run_timestamp_seconds` + labels + ` 1.7013e+09`,
		}},
		"failed": {`{"store":"store1","last-run-state":"disk full","last-run-endtime":1700600500,"next-run":1701200000,` + successful + `}`, []string{
			`pbs_job_last_run_end_timestamp_seconds` + labels + ` 1.7006005e+09`,
			`pbs_job_last_run_success` + labels + ` 0`,
			`pbs_job_next_run_timestamp_seconds` + labels + ` 1.7012e+09`,
		}},
	} {
		srv := newFakePBS(t, map[string]fakeResp{
			"/api2/json/admin/gc":     {body: `{"data":[` + tc.gc + `]}`},
			"/api2/json/admin/verify": {body: `{"data":[]}`},
			"/api2/json/admin/prune":  {body: `{"data":[]}`},
		})
		got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"sync": false}}),
			"pbs_job_last_run_success", "pbs_job_last_run_start_timestamp_seconds", "pbs_job_last_run_end_timestamp_seconds",
			"pbs_job_next_run_timestamp_seconds")
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
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

// A sync job without sync-direction (PBS treats it as pull) is exported with
// the direction PBS reports: none.
func TestSyncJobWithoutDirection(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[{"id":"s-1","store":"store1","remote":"pbs-b","remote-store":"main"}]}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_sync_job_info")
	if len(got) != 1 || !strings.Contains(got[0], `sync_direction=""`) {
		t.Errorf("got %v", got)
	}
}
