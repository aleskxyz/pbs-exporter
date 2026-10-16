package main

import (
	"net/url"
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
# HELP pbs_job_last_run_success Whether the last run of the job ended OK.
# TYPE pbs_job_last_run_success gauge
pbs_job_last_run_success{job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config",store="store1"} 0
pbs_job_last_run_success{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config",store="store1"} 1
pbs_job_last_run_success{job_id="store1",job_type="gc",pbs_namespace="",source="config",store="store1"} 1
pbs_job_last_run_success{job_id="v-1",job_type="verify",pbs_namespace="",source="config",store="store1"} 1
# HELP pbs_job_last_run_end_timestamp_seconds End of the last run of the job.
# TYPE pbs_job_last_run_end_timestamp_seconds gauge
pbs_job_last_run_end_timestamp_seconds{job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config",store="store1"} 1700000060
pbs_job_last_run_end_timestamp_seconds{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config",store="store1"} 1700000300
pbs_job_last_run_end_timestamp_seconds{job_id="store1",job_type="gc",pbs_namespace="",source="config",store="store1"} 1700001000
pbs_job_last_run_end_timestamp_seconds{job_id="v-1",job_type="verify",pbs_namespace="",source="config",store="store1"} 1700003600
# HELP pbs_job_next_run_timestamp_seconds Next scheduled run of the job.
# TYPE pbs_job_next_run_timestamp_seconds gauge
pbs_job_next_run_timestamp_seconds{job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config",store="store1"} 1700086400
pbs_job_next_run_timestamp_seconds{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config",store="store1"} 1700003600
pbs_job_next_run_timestamp_seconds{job_id="store1",job_type="gc",pbs_namespace="",source="config",store="store1"} 1700604800
pbs_job_next_run_timestamp_seconds{job_id="v-1",job_type="verify",pbs_namespace="",source="config",store="store1"} 1700604800
pbs_job_next_run_timestamp_seconds{job_id="v-2",job_type="verify",pbs_namespace="site-a",source="config",store="store1"} 1700100000
# HELP pbs_job_interval_seconds Time between the start of the last run and the next run of the job.
# TYPE pbs_job_interval_seconds gauge
pbs_job_interval_seconds{job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config",store="store1"} 86400
pbs_job_interval_seconds{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config",store="store1"} 3600
pbs_job_interval_seconds{job_id="store1",job_type="gc",pbs_namespace="",source="config",store="store1"} 604800
pbs_job_interval_seconds{job_id="v-1",job_type="verify",pbs_namespace="",source="config",store="store1"} 604800
# HELP pbs_job_last_run_duration_seconds Duration of the last run of the job.
# TYPE pbs_job_last_run_duration_seconds gauge
pbs_job_last_run_duration_seconds{job_id="p-1",job_type="prune",pbs_namespace="site-a",source="config",store="store1"} 60
pbs_job_last_run_duration_seconds{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="config",store="store1"} 300
pbs_job_last_run_duration_seconds{job_id="store1",job_type="gc",pbs_namespace="",source="config",store="store1"} 1000
pbs_job_last_run_duration_seconds{job_id="v-1",job_type="verify",pbs_namespace="",source="config",store="store1"} 3600
# HELP pbs_verify_job_info Settings of a verify job, always 1.
# TYPE pbs_verify_job_info gauge
pbs_verify_job_info{ignore_verified="true",job_id="v-1",max_depth="",outdated_after="30",pbs_namespace="",schedule="sat 02:00",store="store1"} 1
pbs_verify_job_info{ignore_verified="",job_id="v-2",max_depth="",outdated_after="",pbs_namespace="site-a",schedule="daily",store="store1"} 1
# HELP pbs_prune_job_keep Retention setting of a prune job.
# TYPE pbs_prune_job_keep gauge
pbs_prune_job_keep{job_id="p-1",keep="daily",pbs_namespace="site-a",store="store1"} 7
pbs_prune_job_keep{job_id="p-1",keep="last",pbs_namespace="site-a",store="store1"} 3
# HELP pbs_sync_job_info Settings of a sync job, always 1. With source="tasks" only remote and remote_store are known.
# TYPE pbs_sync_job_info gauge
pbs_sync_job_info{group_filter="type:vm,regex:^1",job_id="s-1",pbs_namespace="site-a",remote="pbs-b",remote_ns="site-a",remote_store="main",remove_vanished="true",schedule="hourly",source="config",store="store1",sync_direction="pull",transfer_last="2"} 1
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
		"/api2/json/access/permissions":            {body: `{"data":{"/":{"Datastore.Audit":true},"/remote":{"Datastore.Audit":true,"Sys.Audit":true}}}`},
		"/api2/json/admin/sync?sync-direction=all": {body: `{"data":[]}`},
		"/api2/json/config/remote":                 {body: `{"data":[]}`},
	})
	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"jobs": false}})
	compare(t, e, `
# HELP pbs_job_last_run_success Whether the last run of the job ended OK.
# TYPE pbs_job_last_run_success gauge
pbs_job_last_run_success{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="tasks",store="store1"} 0
pbs_job_last_run_success{job_id="s-2",job_type="sync",pbs_namespace="",source="tasks",store="store1"} 1
# HELP pbs_job_last_run_end_timestamp_seconds End of the last run of the job.
# TYPE pbs_job_last_run_end_timestamp_seconds gauge
pbs_job_last_run_end_timestamp_seconds{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="tasks",store="store1"} 1700110100
pbs_job_last_run_end_timestamp_seconds{job_id="s-2",job_type="sync",pbs_namespace="",source="tasks",store="store1"} 1700001060
# HELP pbs_job_last_run_duration_seconds Duration of the last run of the job.
# TYPE pbs_job_last_run_duration_seconds gauge
pbs_job_last_run_duration_seconds{job_id="s-1",job_type="sync",pbs_namespace="site-a",source="tasks",store="store1"} 100
pbs_job_last_run_duration_seconds{job_id="s-2",job_type="sync",pbs_namespace="",source="tasks",store="store1"} 60
# HELP pbs_sync_job_info Settings of a sync job, always 1. With source="tasks" only remote and remote_store are known.
# TYPE pbs_sync_job_info gauge
pbs_sync_job_info{group_filter="",job_id="s-1",pbs_namespace="site-a",remote="pbs-b",remote_ns="",remote_store="main",remove_vanished="",schedule="",source="tasks",store="store1",sync_direction="",transfer_last=""} 1
pbs_sync_job_info{group_filter="",job_id="s-2",pbs_namespace="",remote="pbs-c",remote_ns="",remote_store="other",remove_vanished="",schedule="",source="tasks",store="store1",sync_direction="",transfer_last=""} 1
# HELP pbs_collector_success Whether a collector succeeded in the last scrape.
# TYPE pbs_collector_success gauge
pbs_collector_success{collector="datastores"} 1
pbs_collector_success{collector="groups"} 1
pbs_collector_success{collector="node"} 1
pbs_collector_success{collector="snapshots"} 1
pbs_collector_success{collector="sync"} 0
pbs_collector_success{collector="tasks"} 1
pbs_collector_success{collector="version"} 1
`, "pbs_job_last_run_success", "pbs_job_last_run_end_timestamp_seconds", "pbs_job_next_run_timestamp_seconds",
		"pbs_job_last_run_duration_seconds", "pbs_sync_job_info", "pbs_remote_info", "pbs_collector_success")
	if !strings.Contains(logs.String(), "missing privilege Remote.Audit on /remote") {
		t.Errorf("the log does not name the missing privilege:\n%s", logs)
	}
	for _, r := range srv.Requests() {
		if strings.Contains(r, "/admin/sync") || strings.Contains(r, "/config/remote") {
			t.Errorf("unexpected request %s without Remote.Audit", r)
		}
	}
}

func TestHasPrivilege(t *testing.T) {
	perms := map[string]map[string]any{
		"/":               {"Sys.Audit": true},
		"/remote/pbs-b":   {"Remote.Audit": false},
		"/remotely-other": {"Remote.Audit": true},
	}
	if !hasPrivilege(perms, "/remote", "Remote.Audit") {
		t.Error("privilege on a path below /remote must count")
	}
	delete(perms, "/remote/pbs-b")
	if hasPrivilege(perms, "/remote", "Remote.Audit") {
		t.Error("a path that only starts with /remote must not count")
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
	e := testExporter(t, srv, ModuleConfig{TaskLookback: "48h", TaskLimit: 2})
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
