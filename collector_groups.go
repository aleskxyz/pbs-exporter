package main

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"

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
		"Time of the newest finished snapshot of the backup group.")
	groupLastBackupVerifyState = groupDesc("last_backup_verify_state",
		"Verification state (ok, failed, none) of the newest finished snapshot of the backup group, always 1.", "state")
	groupInfo = groupDesc("info",
		"Owner and comment (group notes, otherwise the notes of the newest snapshot) of the backup group, always 1.", "owner", "comment")
	groupOldestBackupTimestamp = groupDesc("oldest_backup_timestamp_seconds",
		"Time of the oldest snapshot of the backup group.")
	groupProtectedSnapshots = groupDesc("protected_snapshots",
		"Number of protected snapshots of the backup group.")
	groupLastBackupSize = groupDesc("last_backup_size_bytes",
		"Size of the newest snapshot of the backup group.")
	groupSnapshotsByVerifyState = groupDesc("snapshots_by_verify_state",
		"Number of snapshots of the backup group per verification state (ok, failed, none).", "state")
	groupLastVerifyTimestamp = groupDesc("last_verify_timestamp_seconds",
		"Start of the newest verification of any snapshot of the backup group (synced snapshots keep the verification of their source).")
	groupLastBackupEncrypted = groupDesc("last_backup_encrypted",
		"Whether the newest snapshot of the backup group is encrypted.")
	groupBackupInterval = groupDesc("backup_interval_seconds",
		"Typical time between snapshots of the backup group: the median of the gaps between its newest 4 snapshots; omitted below 3 snapshots.")
	groupLastBackupTaskSuccess = groupDesc("last_backup_task_success",
		"Whether the newest finished backup task of the backup group ended OK.")
	groupLastBackupTaskEnd = groupDesc("last_backup_task_end_timestamp_seconds",
		"End of the newest finished backup task of the backup group.")
	groupLastBackupDuration = groupDesc("last_backup_duration_seconds",
		"Duration of the newest finished backup task of the backup group.")
)

// intervalSnapshots is how many of the newest snapshots the backup interval is
// measured on: retention thins out older snapshots, so older gaps are longer.
const intervalSnapshots = 4

// minIntervalSnapshots is the number of snapshots a group needs for an interval.
const minIntervalSnapshots = 3

// verifyStates are always exported by pbs_group_snapshots_by_verify_state.
var verifyStates = []string{"ok", "failed", "none"}

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
	// newest is the newest finished snapshot, nil if there is none; newest,
	// oldest and times only count finished snapshots.
	newest       *snapshotItem
	oldest       int64
	protected    int
	verifyStates map[string]int
	// lastVerify is the start of the newest verification, 0 if none
	lastVerify int64
	times      []int64
}

// finished reports whether a snapshot has a manifest; PBS lists running
// backups without one, and without notes, verification and size.
func (s snapshotItem) finished() bool {
	return s.Size != nil
}

// interval returns the median of the gaps between the newest snapshots.
func (g *groupSummary) interval() (int64, bool) {
	if len(g.times) < minIntervalSnapshots {
		return 0, false
	}
	times := slices.Clone(g.times)
	slices.SortFunc(times, func(a, b int64) int { return cmp.Compare(b, a) })
	times = times[:min(len(times), intervalSnapshots)]
	gaps := make([]int64, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i-1]-times[i])
	}
	slices.Sort(gaps)
	n := len(gaps)
	if n%2 == 1 {
		return gaps[n/2], true
	}
	return (gaps[n/2-1] + gaps[n/2]) / 2, true
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
			g.times = append(g.times, snap.BackupTime)
		}
		if snap.Protected {
			g.protected++
		}
		g.verifyStates[verifyState(snap)]++
		if start, ok := parseUPIDStart(snap.Verification.UPID); ok {
			g.lastVerify = max(g.lastVerify, start)
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

// namespaceGroups are the groups of one namespace with their snapshot summaries.
type namespaceGroups struct {
	store, ns string
	groups    []groupItem
	summaries map[groupKey]*groupSummary
}

var groupsCollector = &collector{
	name:      "groups",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore, Sys.Audit on /system/tasks for the backup tasks",
	descs: []*prometheus.Desc{
		groupSnapshots, groupLastBackupTimestamp, groupLastBackupVerifyState, groupInfo,
		groupOldestBackupTimestamp, groupProtectedSnapshots, groupLastBackupSize, groupSnapshotsByVerifyState,
		groupLastVerifyTimestamp, groupLastBackupEncrypted, groupBackupInterval,
		groupLastBackupTaskSuccess, groupLastBackupTaskEnd, groupLastBackupDuration,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var mu sync.Mutex
		var all []namespaceGroups
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
			mu.Lock()
			all = append(all, namespaceGroups{store, ns, groups, summaries})
			mu.Unlock()
			return snapErr
		})

		// the backup tasks are matched to groups over all namespaces of a store
		tasks, taskErr := s.getTasks(ctx)
		lastTasks := matchBackupTasks(all, tasks)
		for _, nsg := range all {
			collectGroupMetrics(nsg, lastTasks, ch)
		}
		return errors.Join(nsErr, taskErr)
	},
}

func collectGroupMetrics(nsg namespaceGroups, lastTasks map[groupRef]taskItem, ch chan<- prometheus.Metric) {
	root := namespaceRoot(nsg.ns)
	seen := map[groupKey]bool{}
	for _, g := range nsg.groups {
		key := groupKey{g.BackupType, g.BackupID}
		// a group listed twice would make the whole scrape fail
		if seen[key] {
			continue
		}
		seen[key] = true
		labels := []string{nsg.store, nsg.ns, root, g.BackupType, g.BackupID}
		gauge := func(d *prometheus.Desc, v float64, extra ...string) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, append(labels, extra...)...)
		}

		gauge(groupSnapshots, float64(g.BackupCount))
		comment := g.Comment
		// no timestamp for a group without finished snapshot: 0 reads as 1970
		infoKnown := true
		if nsg.summaries == nil {
			// without the snapshot list, the group list tells the newest
			// finished snapshot only for groups with more than one: the only
			// snapshot of a group may still be running
			if g.LastBackup > 0 && g.BackupCount > 1 {
				gauge(groupLastBackupTimestamp, float64(g.LastBackup))
			}
			// the notes of the newest snapshot are unknown; a series with an
			// empty comment would replace the usual one for a scrape
			infoKnown = comment != ""
		}
		if summary := nsg.summaries[key]; summary != nil {
			if newest := summary.newest; newest != nil {
				if comment == "" {
					comment = newest.Comment
				}
				gauge(groupLastBackupTimestamp, float64(newest.BackupTime))
				gauge(groupLastBackupVerifyState, 1, verifyState(*newest))
				gauge(groupOldestBackupTimestamp, float64(summary.oldest))
				gauge(groupLastBackupSize, float64(*newest.Size))
				encrypted := 0.0
				if newest.Fingerprint != "" {
					encrypted = 1
				}
				gauge(groupLastBackupEncrypted, encrypted)
				if interval, ok := summary.interval(); ok {
					gauge(groupBackupInterval, float64(interval))
				}
			}
			gauge(groupProtectedSnapshots, float64(summary.protected))
			for _, state := range verifyStates {
				gauge(groupSnapshotsByVerifyState, float64(summary.verifyStates[state]), state)
			}
			for state, n := range summary.verifyStates {
				if !slices.Contains(verifyStates, state) {
					gauge(groupSnapshotsByVerifyState, float64(n), state)
				}
			}
			if summary.lastVerify > 0 {
				gauge(groupLastVerifyTimestamp, float64(summary.lastVerify))
			}
		}
		if t, ok := lastTasks[groupRef{nsg.store, nsg.ns, g.BackupType, g.BackupID}]; ok {
			success := 0.0
			if t.Status == "OK" {
				success = 1
			}
			gauge(groupLastBackupTaskSuccess, success)
			gauge(groupLastBackupTaskEnd, float64(t.EndTime))
			gauge(groupLastBackupDuration, float64(t.EndTime-t.StartTime))
		}
		if infoKnown {
			gauge(groupInfo, 1, g.Owner, comment)
		}
	}
}

// groupRef identifies a backup group across namespaces.
type groupRef struct {
	store, ns, backupType, backupID string
}

// backupTaskKey is what the worker ID of a backup task says about its group:
// "<store>:<type>/<id>", without the namespace.
type backupTaskKey struct {
	store, backupType, backupID string
}

func parseBackupWorkerID(workerID string) (backupTaskKey, bool) {
	store, group, ok := strings.Cut(workerID, ":")
	if !ok {
		return backupTaskKey{}, false
	}
	backupType, backupID, ok := strings.Cut(group, "/")
	if !ok || store == "" || backupType == "" || backupID == "" {
		return backupTaskKey{}, false
	}
	return backupTaskKey{store, backupType, backupID}, true
}

// authUser returns the user of an auth ID, without the token name.
func authUser(authID string) string {
	user, _, _ := strings.Cut(authID, "!")
	return user
}

// matchBackupTasks returns the newest finished backup task of each group.
// The worker ID of a backup task has no namespace. If only one namespace of
// the store has a group with that type and ID, the task is its, whoever ran
// it (e.g. root@pam can write to groups it does not own). Otherwise the task
// user must be the group owner: exactly, then without the token name; a
// group whose task can not be told apart is left out.
func matchBackupTasks(all []namespaceGroups, tasks []taskItem) map[groupRef]taskItem {
	candidates := map[backupTaskKey][]groupRef{}
	owners := map[groupRef]string{}
	for _, nsg := range all {
		for _, g := range nsg.groups {
			key := backupTaskKey{nsg.store, g.BackupType, g.BackupID}
			ref := groupRef{nsg.store, nsg.ns, g.BackupType, g.BackupID}
			candidates[key] = append(candidates[key], ref)
			owners[ref] = g.Owner
		}
	}

	byKey := map[backupTaskKey][]taskItem{}
	for _, t := range tasks {
		if t.WorkerType != "backup" || !t.finished() {
			continue
		}
		if key, ok := parseBackupWorkerID(t.WorkerID); ok {
			byKey[key] = append(byKey[key], t)
		}
	}

	newest := func(tasks []taskItem, keep func(taskItem) bool) (taskItem, bool) {
		var best taskItem
		found := false
		for _, t := range tasks {
			if keep(t) && (!found || t.StartTime > best.StartTime) {
				best, found = t, true
			}
		}
		return best, found
	}

	out := map[groupRef]taskItem{}
	for key, refs := range candidates {
		tasks := byKey[key]
		if len(tasks) == 0 {
			continue
		}
		if len(refs) == 1 {
			if t, ok := newest(tasks, func(taskItem) bool { return true }); ok {
				out[refs[0]] = t
			}
			continue
		}
		for _, ref := range refs {
			owner := owners[ref]
			for _, same := range []func(a, b string) bool{
				func(a, b string) bool { return a == b },
				func(a, b string) bool { return authUser(a) == authUser(b) },
			} {
				shared := false
				for _, other := range refs {
					if other != ref && same(owners[other], owner) {
						shared = true
					}
				}
				t, ok := newest(tasks, func(t taskItem) bool { return same(t.User, owner) })
				if !ok {
					continue
				}
				if !shared {
					out[ref] = t
				}
				// a matching task was found; if it is ambiguous, a looser
				// comparison would not help
				break
			}
		}
	}
	return out
}
