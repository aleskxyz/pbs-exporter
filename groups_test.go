package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
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
# HELP pbs_group_last_backup_timestamp_seconds Time of the newest snapshot of the backup group.
# TYPE pbs_group_last_backup_timestamp_seconds gauge
pbs_group_last_backup_timestamp_seconds{backup_id="100",backup_type="ct",datastore="store1",namespace_root="",pbs_namespace=""} 1700000000
pbs_group_last_backup_timestamp_seconds{backup_id="100",backup_type="vm",datastore="store1",namespace_root="",pbs_namespace=""} 1700086400
pbs_group_last_backup_timestamp_seconds{backup_id="200",backup_type="vm",datastore="store1",namespace_root="site-a",pbs_namespace="site-a"} 1700000000
# HELP pbs_group_last_backup_verify_state Verification state (ok, failed, none) of the newest snapshot of the backup group, always 1.
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
pbs_collector_success{collector="node"} 1
pbs_collector_success{collector="version"} 1
`, "pbs_collector_success")
}

func TestNamespaceRoot(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a/b/c": "a"} {
		if got := namespaceRoot(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
