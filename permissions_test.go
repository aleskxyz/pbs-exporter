package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

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
}

// Without Sys.Audit on /system/tasks, PBS lists only the token's own tasks
// without an error; the tasks collector reports it.
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

// Without the permissions, a complete list can not be told apart from one PBS
// filtered: every collector that checks privileges fails, the others do not.
func TestPermissionsUnreadable(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {status: http.StatusInternalServerError, body: `{"data":null,"message":"boom"}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true}}), "pbs_collector_success")
	for c, want := range map[string]int{
		"access": 0, "datastores": 0, "groups": 0, "jobs": 0, "node": 0, "sync": 0, "tasks": 0,
		"apt": 1, "time": 1, "version": 1, "zfs": 1,
	} {
		if s := fmt.Sprintf(`pbs_collector_success{collector=%q} %d`, c, want); !slices.Contains(got, s) {
			t.Errorf("want %s, got:\n%s", s, strings.Join(got, "\n"))
		}
	}
}

// Without Datastore.Audit on /datastore, PBS lists only the datastores,
// namespaces and jobs the token can audit, without an error: the collectors
// reading them report it.
func TestFilteredDatastores(t *testing.T) {
	srv := newFakePBS(t, map[string]fakeResp{
		"/api2/json/access/permissions": {body: `{"data":{"/":{"Sys.Audit":true},"/datastore/store1":{"Datastore.Audit":true},"/remote":{"Remote.Audit":true},"/system":{"Sys.Audit":true}}}`},
	})
	got := seriesOf(t, testExporter(t, srv, ModuleConfig{}), "pbs_collector_success")
	for c, want := range map[string]int{"datastores": 0, "groups": 0, "jobs": 0, "sync": 0, "node": 1, "tasks": 1} {
		if s := fmt.Sprintf(`pbs_collector_success{collector=%q} %d`, c, want); !slices.Contains(got, s) {
			t.Errorf("want %s, got:\n%s", s, strings.Join(got, "\n"))
		}
	}
}
