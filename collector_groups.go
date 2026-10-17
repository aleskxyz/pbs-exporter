package main

import (
	"cmp"
	"context"
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
		"Whether the archives of the newest snapshot of the backup group are encrypted (signed only does not count).")
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

// encrypted reports whether all archives of the manifest of a snapshot are
// encrypted. The manifest itself is only signed, files that are not in the
// manifest (the client log) have no crypt mode, and a key fingerprint is
// also set for backups that are signed, not encrypted ("sign-only").
func (s snapshotItem) encrypted() bool {
	archives := 0
	for _, f := range s.Files {
		if f.Filename == "index.json.blob" || f.CryptMode == "" {
			continue
		}
		if f.CryptMode != "encrypt" {
			return false
		}
		archives++
	}
	return archives > 0
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
		// stores with a namespace whose groups are unknown: their backup tasks
		// could belong to a group that is not seen
		incomplete := map[string]bool{}
		perms, permsErr := s.getPermissions(ctx)
		nsErr := s.forEachNamespace(ctx, func(store, ns string) error {
			groups, err := s.getGroups(ctx, store, ns)
			if err != nil {
				mu.Lock()
				incomplete[store] = true
				mu.Unlock()
				return err
			}
			// the group list stays useful without the snapshots
			snapshots, snapErr := s.getSnapshots(ctx, store, ns)
			var summaries map[groupKey]*groupSummary
			if snapErr == nil {
				summaries = summarizeSnapshots(snapshots)
			}
			mu.Lock()
			all = append(all, namespaceGroups{store, ns, uniqueGroups(groups), summaries})
			// PBS leaves out namespaces without privileges: without
			// Datastore.Audit on the whole datastore, groups may be unseen
			if permsErr != nil || !perms.canAuditDatastore(store) {
				incomplete[store] = true
			}
			mu.Unlock()
			return snapErr
		})

		// the backup tasks are matched to groups over all namespaces of a store
		tasks, taskErr := s.getTasks(ctx)
		lastTasks := matchBackupTasks(all, tasks, incomplete)
		for _, nsg := range all {
			collectGroupMetrics(nsg, lastTasks, ch)
		}
		return joinUnique(nsErr, taskErr, permsErr)
	},
}

func collectGroupMetrics(nsg namespaceGroups, lastTasks map[groupRef]taskItem, ch chan<- prometheus.Metric) {
	root := namespaceRoot(nsg.ns)
	for _, g := range nsg.groups {
		key := groupKey{g.BackupType, g.BackupID}
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
				if newest.encrypted() {
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
			if succeeded(t.Status) {
				success = 1
			}
			gauge(groupLastBackupTaskSuccess, success)
			gauge(groupLastBackupTaskEnd, float64(t.EndTime))
			// a clock step could make it negative
			gauge(groupLastBackupDuration, float64(max(t.EndTime-t.StartTime, 0)))
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

// canWrite reports whether a task of authID can have created a snapshot in
// a group owned by owner: PBS only lets the owner, or the user of an owning
// API token, add snapshots to a group (there is no exception for root@pam).
func canWrite(authID, owner string) bool {
	if authID == owner {
		return true
	}
	user, _, isToken := strings.Cut(owner, "!")
	return isToken && user == authID
}

// matchBackupTasks returns the newest finished backup task of each group.
// The worker ID of a backup task has no namespace, so a task belongs to the
// group with its type and ID that the task user can write to. A task that
// fits groups in several namespaces is left out, and so are the tasks of
// incomplete stores.
func matchBackupTasks(all []namespaceGroups, tasks []taskItem, incomplete map[string]bool) map[groupRef]taskItem {
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

	return newestPerKey(tasks, func(t taskItem) (groupRef, bool) {
		if t.WorkerType != "backup" {
			return groupRef{}, false
		}
		key, ok := parseBackupWorkerID(t.WorkerID)
		if !ok || incomplete[key.store] {
			return groupRef{}, false
		}
		var match []groupRef
		for _, ref := range candidates[key] {
			if canWrite(t.User, owners[ref]) {
				match = append(match, ref)
			}
		}
		if len(match) != 1 {
			return groupRef{}, false
		}
		return match[0], true
	})
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

// canAuditDatastore reports whether the token sees all namespaces of a
// datastore: Datastore.Audit on it (or /datastore, or "/") that propagates.
func (p permissions) canAuditDatastore(store string) bool {
	return p["/datastore"]["Datastore.Audit"] || p["/datastore/"+store]["Datastore.Audit"]
}
