package main

import (
	"context"
	"net/url"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// groupLabels identify a backup group. They do not depend on free text such
// as notes, which goes on pbs_group_info.
var groupLabels = []string{"datastore", "pbs_namespace", "namespace_root", "backup_type", "backup_id"}

func groupDesc(name, help string, extra ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(promNamespace, "group", name), help,
		append(append([]string(nil), groupLabels...), extra...), nil)
}

var (
	groupSnapshots = groupDesc("snapshots",
		"Number of snapshots of the backup group.")
	groupLastBackupTimestamp = groupDesc("last_backup_timestamp_seconds",
		"Time of the newest snapshot of the backup group.")
	groupLastBackupVerifyState = groupDesc("last_backup_verify_state",
		"Verification state (ok, failed, none) of the newest snapshot of the backup group, always 1.", "state")
	groupInfo = groupDesc("info",
		"Owner and comment (group notes, otherwise the notes of the newest snapshot) of the backup group, always 1.", "owner", "comment")
)

// groupItem is an entry of /admin/datastore/{store}/groups.
type groupItem struct {
	BackupType  string `json:"backup-type"`
	BackupID    string `json:"backup-id"`
	BackupCount int64  `json:"backup-count"`
	LastBackup  int64  `json:"last-backup"`
	Owner       string `json:"owner"`
	Comment     string `json:"comment"`
}

type groupKey struct {
	backupType, backupID string
}

// groupSummary is what the snapshot list says about one backup group.
type groupSummary struct {
	// newest is the newest finished snapshot, nil if there is none.
	newest *snapshotItem
}

// finished reports whether a snapshot has a manifest; PBS lists running
// backups without one, and without notes, verification and size.
func (s snapshotItem) finished() bool {
	return s.Size != nil
}

// summarizeSnapshots goes once over the snapshot list of a namespace and
// returns the summary of every backup group in it.
func summarizeSnapshots(snapshots []snapshotItem) map[groupKey]*groupSummary {
	groups := map[groupKey]*groupSummary{}
	for i, snap := range snapshots {
		key := groupKey{snap.BackupType, snap.BackupID}
		g, ok := groups[key]
		if !ok {
			g = &groupSummary{}
			groups[key] = g
		}
		if snap.finished() && (g.newest == nil || snap.BackupTime > g.newest.BackupTime) {
			g.newest = &snapshots[i]
		}
	}
	return groups
}

// verifyState returns the verification state of a snapshot: "ok", "failed"
// or "none" if it was never verified.
func verifyState(snap snapshotItem) string {
	if snap.Verification.State == "" {
		return "none"
	}
	return snap.Verification.State
}

// namespaceRoot returns the top-level namespace of ns ("" for the root).
func namespaceRoot(ns string) string {
	root, _, _ := strings.Cut(ns, "/")
	return root
}

func (s *scrapeState) getGroups(ctx context.Context, store, ns string) ([]groupItem, error) {
	var out []groupItem
	err := s.api.get(ctx, apiPath("admin", "datastore", store, "groups"), url.Values{"ns": {ns}}, &out)
	return out, err
}

var groupsCollector = &collector{
	name:      "groups",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs:     []*prometheus.Desc{groupSnapshots, groupLastBackupTimestamp, groupLastBackupVerifyState, groupInfo},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return s.forEachNamespace(ctx, func(store, ns string) error {
			groups, err := s.getGroups(ctx, store, ns)
			if err != nil {
				return err
			}
			// the group list stays useful without the snapshots
			snapshots, snapErr := s.getSnapshots(ctx, store, ns)
			collectGroupMetrics(store, ns, groups, summarizeSnapshots(snapshots), ch)
			return snapErr
		})
	},
}

func collectGroupMetrics(store, ns string, groups []groupItem, summaries map[groupKey]*groupSummary, ch chan<- prometheus.Metric) {
	root := namespaceRoot(ns)
	seen := map[groupKey]bool{}
	for _, g := range groups {
		key := groupKey{g.BackupType, g.BackupID}
		// a group listed twice would make the whole scrape fail
		if seen[key] {
			continue
		}
		seen[key] = true
		labels := []string{store, ns, root, g.BackupType, g.BackupID}
		gauge := func(d *prometheus.Desc, v float64, extra ...string) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, append(labels, extra...)...)
		}
		summary := summaries[key]

		gauge(groupSnapshots, float64(g.BackupCount))
		// no series for a group without snapshots: 0 would read as 1970
		if g.LastBackup > 0 {
			gauge(groupLastBackupTimestamp, float64(g.LastBackup))
		}
		comment := g.Comment
		if summary != nil && summary.newest != nil {
			gauge(groupLastBackupVerifyState, 1, verifyState(*summary.newest))
			if comment == "" {
				comment = summary.newest.Comment
			}
		}
		gauge(groupInfo, 1, g.Owner, comment)
	}
}
