package main

import (
	"context"
	"net/url"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// groupLabels identify a backup group. They do not depend on free text such
// as notes, which goes on pbs_group_info.
var groupLabels = []string{"datastore", "datastore_namespace", "backup_type", "backup_id"}

func groupDesc(name, help string, extra ...string) *prometheus.Desc {
	return newDesc("group", name, help, append(slices.Clone(groupLabels), extra...)...)
}

var (
	groupSnapshots = groupDesc("snapshots",
		"Number of snapshots of the backup group (backup-count).")
	groupLastBackupTimestamp = groupDesc("last_backup_timestamp_seconds",
		"Time of the newest finished snapshot of the backup group.")
	groupLastBackupVerifyState = groupDesc("last_backup_verify_state",
		"Verification state of the newest finished snapshot of the backup group as PBS reports it (ok, failed; empty when never verified), always 1.", "state")
	groupInfo = groupDesc("info",
		"Owner and comment (first line of the group notes) of the backup group, always 1.", "owner", "comment")
	groupOldestBackupTimestamp = groupDesc("oldest_backup_timestamp_seconds",
		"Time of the oldest finished snapshot of the backup group.")
	groupProtectedSnapshots = groupDesc("protected_snapshots",
		"Number of protected snapshots of the backup group; omitted when there are none.")
	groupLastBackupSize = groupDesc("last_backup_size_bytes",
		"Size of the newest finished snapshot of the backup group.")
	groupSnapshotsByVerifyState = groupDesc("snapshots_by_verify_state",
		"Number of snapshots of the backup group per verification state as PBS reports it (ok, failed; empty when never verified); omitted for states without snapshots.", "state")
	groupLastVerifyTimestamp = groupDesc("last_verify_timestamp_seconds",
		"Start of the newest verification of any snapshot of the backup group (synced snapshots keep the verification of their source).")
)

// groupItem is an entry of /admin/datastore/{store}/groups.
type groupItem struct {
	BackupType  string `json:"backup-type"`
	BackupID    string `json:"backup-id"`
	BackupCount *int64 `json:"backup-count"`
	Owner       string `json:"owner"`
	Comment     string `json:"comment"`
}

type groupKey struct {
	backupType, backupID string
}

// groupSummary is what the snapshot list says about one backup group.
type groupSummary struct {
	// newest is the newest finished snapshot, nil if there is none; newest
	// and oldest only count finished snapshots.
	newest       *snapshotItem
	oldest       int64
	protected    int
	verifyStates map[string]int
	// lastVerify is the start of the newest verification, 0 if none
	lastVerify int64
}

// summarizeSnapshots goes once over the snapshot list of a namespace and
// returns the summary of every backup group in it.
func summarizeSnapshots(snapshots []snapshotItem) map[groupKey]*groupSummary {
	groups := map[groupKey]*groupSummary{}
	for i, snap := range snapshots {
		key := groupKey{snap.BackupType, snap.BackupID}
		g, ok := groups[key]
		if !ok {
			g = &groupSummary{verifyStates: map[string]int{}}
			groups[key] = g
		}
		if snap.finished() {
			if g.newest == nil || snap.BackupTime > g.newest.BackupTime {
				g.newest = &snapshots[i]
			}
			if g.oldest == 0 || snap.BackupTime < g.oldest {
				g.oldest = snap.BackupTime
			}
		}
		if snap.Protected {
			g.protected++
		}
		g.verifyStates[snap.Verification.State]++
		if start, ok := parseUPIDStart(snap.Verification.UPID); ok {
			g.lastVerify = max(g.lastVerify, start)
		}
	}
	return groups
}

func (s *scrapeState) getGroups(ctx context.Context, store, ns string) ([]groupItem, error) {
	var out []groupItem
	err := s.api.get(ctx, apiPath("admin", "datastore", store, "groups"), url.Values{"ns": {ns}}, &out)
	return out, err
}

// namespaceGroups are the groups of one namespace with their snapshot summaries.
type namespaceGroups struct {
	store, ns string
	groups    []groupItem
	summaries map[groupKey]*groupSummary
}

var groupsCollector = &collector{
	name:      "groups",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs: []*prometheus.Desc{
		groupSnapshots, groupLastBackupTimestamp, groupLastBackupVerifyState, groupInfo,
		groupOldestBackupTimestamp, groupProtectedSnapshots, groupLastBackupSize, groupSnapshotsByVerifyState,
		groupLastVerifyTimestamp,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		nsErr := s.forEachNamespace(ctx, func(store, ns string) error {
			groups, err := s.getGroups(ctx, store, ns)
			if err != nil {
				return err
			}
			// the group list stays useful without the snapshots
			snapshots, snapErr := s.getSnapshots(ctx, store, ns)
			var summaries map[groupKey]*groupSummary
			if snapErr == nil {
				summaries = summarizeSnapshots(snapshots)
			}
			collectGroupMetrics(namespaceGroups{store, ns, uniqueGroups(groups), summaries}, ch)
			return snapErr
		})
		// PBS leaves out the datastores and namespaces the token can not audit
		return joinUnique(nsErr, s.privilegeGap(ctx, permissions.canAuditDatastores, errDatastoresFiltered))
	},
}

func collectGroupMetrics(nsg namespaceGroups, ch chan<- prometheus.Metric) {
	for _, g := range nsg.groups {
		key := groupKey{g.BackupType, g.BackupID}
		labels := []string{nsg.store, nsg.ns, g.BackupType, g.BackupID}
		send := func(d *prometheus.Desc, v float64, extra ...string) {
			gauge(ch, d, v, append(labels, extra...)...)
		}

		if g.BackupCount != nil {
			send(groupSnapshots, float64(*g.BackupCount))
		}
		send(groupInfo, 1, g.Owner, g.Comment)
		// without the snapshot list (nil summaries), only the group list is
		// exported
		summary := nsg.summaries[key]
		if summary == nil {
			continue
		}
		if newest := summary.newest; newest != nil {
			send(groupLastBackupTimestamp, float64(newest.BackupTime))
			send(groupLastBackupVerifyState, 1, newest.Verification.State)
			send(groupOldestBackupTimestamp, float64(summary.oldest))
			send(groupLastBackupSize, float64(*newest.Size))
		}
		if summary.protected > 0 {
			send(groupProtectedSnapshots, float64(summary.protected))
		}
		for state, n := range summary.verifyStates {
			send(groupSnapshotsByVerifyState, float64(n), state)
		}
		if summary.lastVerify > 0 {
			send(groupLastVerifyTimestamp, float64(summary.lastVerify))
		}
	}
}

// uniqueGroups returns groups without the groups listed more than once,
// which would make the whole scrape fail with duplicate series.
func uniqueGroups(groups []groupItem) []groupItem {
	seen := map[groupKey]bool{}
	var out []groupItem
	for _, g := range groups {
		if key := (groupKey{g.BackupType, g.BackupID}); !seen[key] {
			seen[key] = true
			out = append(out, g)
		}
	}
	return out
}
