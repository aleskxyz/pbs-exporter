package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	syncJobInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "sync_job", "info"),
		"Settings of a sync job, always 1. With source=\"tasks\" only datastore, pbs_namespace, remote and remote_store are known.",
		[]string{"job_id", "datastore", "pbs_namespace", "source", "remote", "remote_store", "remote_ns",
			"sync_direction", "remove_vanished", "transfer_last", "group_filter", "schedule"}, nil,
	)
	remoteInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "remote", "info"),
		"A remote PBS that sync jobs read from or write to, always 1.",
		[]string{"remote", "host"}, nil,
	)
)

type syncJob struct {
	ID             string   `json:"id"`
	Store          string   `json:"store"`
	Namespace      string   `json:"ns"`
	Remote         string   `json:"remote"`
	RemoteStore    string   `json:"remote-store"`
	RemoteNS       string   `json:"remote-ns"`
	SyncDirection  string   `json:"sync-direction"`
	RemoveVanished *bool    `json:"remove-vanished"`
	TransferLast   *int64   `json:"transfer-last"`
	GroupFilter    []string `json:"group-filter"`
	Schedule       string   `json:"schedule"`
	jobStatus
}

type remoteConfig struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

// hasRemoteAudit reports whether the token has Remote.Audit on /remote and
// it propagates to the remotes below (also when granted on "/"), so that
// /admin/sync lists the jobs of all remotes. Remote.Audit on a single remote
// (/remote/<name>) only shows the jobs of that remote.
func (p permissions) hasRemoteAudit() bool {
	return p["/remote"]["Remote.Audit"]
}

// canAuditRemote reports whether the token has Remote.Audit on the given
// remote itself.
func (p permissions) canAuditRemote(remote string) bool {
	_, ok := p["/remote/"+remote]["Remote.Audit"]
	return ok
}

var syncCollector = &collector{
	name:      "sync",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore, Remote.Audit on /remote, Sys.Audit on /system/tasks",
	descs: []*prometheus.Desc{
		jobLastRunSuccess, jobLastRunEnd, jobNextRun, jobInterval, jobLastRunDuration,
		syncJobInfo, remoteInfo,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		// PBS leaves out the jobs and remotes the token may not see, without
		// an error: the jobs of a remote need Remote.Audit on that remote
		configured, jobsErr := collectSyncJobs(ctx, s, ch)
		errs := []error{jobsErr, collectRemotes(ctx, s, ch)}
		perms, permsErr := s.getPermissions(ctx)
		switch {
		case permsErr != nil:
			// without the permissions, hidden jobs can not be told apart
			errs = append(errs, permsErr)
		case jobsErr != nil:
			// without the job list, every job in the task log would look hidden
		case !perms.hasRemoteAudit():
			hidden, err := collectSyncJobsFromTasks(ctx, s, ch, configured, perms)
			errs = append(errs, err)
			if hidden > 0 {
				errs = append(errs, fmt.Errorf("missing privilege Remote.Audit on /remote: %d sync jobs are not readable, their last run is taken from the task log", hidden))
			}
		}
		return joinUnique(errs...)
	},
}

// collectSyncJobs exports the sync jobs PBS lists and returns their IDs.
func collectSyncJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) (map[string]bool, error) {
	var jobs []syncJob
	if err := s.api.get(ctx, apiPath("admin", "sync"), url.Values{"sync-direction": {"all"}}, &jobs); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	var errs []error
	for _, j := range jobs {
		ids[j.ID] = true
		direction := j.SyncDirection
		if direction == "" {
			direction = "pull"
		}
		run, err := s.withPreviousRun(ctx, j.run())
		errs = append(errs, err)
		collectJobRun(ch, run, "sync", j.ID, j.Store, j.Namespace, "config")
		ch <- prometheus.MustNewConstMetric(syncJobInfo, prometheus.GaugeValue, 1,
			j.ID, j.Store, j.Namespace, "config", j.Remote, j.RemoteStore, j.RemoteNS,
			direction, optBool(j.RemoveVanished), optInt(j.TransferLast), strings.Join(j.GroupFilter, ","), j.Schedule)
	}
	return ids, joinUnique(errs...)
}

func collectRemotes(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var remotes []remoteConfig
	if err := s.api.get(ctx, apiPath("config", "remote"), nil, &remotes); err != nil {
		return err
	}
	for _, r := range remotes {
		ch <- prometheus.MustNewConstMetric(remoteInfo, prometheus.GaugeValue, 1, r.Name, r.Host)
	}
	return nil
}

// collectSyncJobsFromTasks derives the last run of each hidden sync job from
// its newest syncjob task, and returns how many it found. A job is hidden if
// it is not in configured and its remote is one the token can not audit; a
// job of a remote the token can audit, or between local datastores, that is
// not listed was deleted. The next run and the settings stay unknown, and a
// job whose last run is older than the task log is not seen.
func collectSyncJobsFromTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric, configured map[string]bool, perms permissions) (int, error) {
	tasks, err := s.getTasks(ctx)
	if !usableTasks(err) {
		return 0, err
	}
	newest := newestPerKey(tasks, func(t taskItem) (string, bool) {
		if t.WorkerType != "syncjob" {
			return "", false
		}
		id, ok := parseSyncWorkerID(t.WorkerID)
		return id.jobID, ok && !configured[id.jobID] && id.remote != "" && !perms.canAuditRemote(id.remote)
	})
	for jobID, t := range newest {
		id, _ := parseSyncWorkerID(t.WorkerID)
		run := jobRun{state: t.Status, start: t.StartTime, end: t.EndTime, duration: -1}
		collectJobRun(ch, run, "sync", jobID, id.store, id.ns, "tasks")
		ch <- prometheus.MustNewConstMetric(syncJobInfo, prometheus.GaugeValue, 1,
			jobID, id.store, id.ns, "tasks", id.remote, id.remoteStore, "", "", "", "", "", "")
	}
	return len(newest), err
}
