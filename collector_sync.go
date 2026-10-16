package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	syncJobInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "sync_job", "info"),
		"Settings of a sync job, always 1. With source=\"tasks\" only remote and remote_store are known.",
		[]string{"job_id", "store", "pbs_namespace", "source", "remote", "remote_store", "remote_ns",
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

// hasRemoteAudit reports whether the permissions of /access/permissions grant
// Remote.Audit on /remote itself (also when propagated from "/"), so that
// /admin/sync lists the jobs of all remotes. Remote.Audit on a single
// remote (/remote/<name>) only shows the jobs of that remote.
func hasRemoteAudit(perms map[string]map[string]any) bool {
	_, ok := perms["/remote"]["Remote.Audit"]
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
		// the map values are the propagate flags, a listed privilege is granted
		var perms map[string]map[string]any
		if err := s.api.get(ctx, apiPath("access", "permissions"), nil, &perms); err != nil {
			return err
		}
		// PBS leaves out the jobs and remotes the token may not see, without
		// an error: jobs between local datastores need no Remote.Audit, the
		// jobs of a remote need it on that remote
		configured, err := collectSyncJobs(ctx, s, ch)
		errs := []error{err, collectRemotes(ctx, s, ch)}
		if !hasRemoteAudit(perms) {
			hidden, err := collectSyncJobsFromTasks(ctx, s, ch, configured)
			errs = append(errs, err)
			if hidden > 0 {
				errs = append(errs, fmt.Errorf("missing privilege Remote.Audit on /remote: %d sync jobs are not readable, their last run is taken from the task log", hidden))
			}
		}
		return errors.Join(errs...)
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
	return ids, errors.Join(errs...)
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

// collectSyncJobsFromTasks derives the last run of each sync job that is not
// in configured from its newest syncjob task, and returns how many it found.
// The next run and the settings stay unknown, and a job whose last run is
// older than the task log is not seen.
func collectSyncJobsFromTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric, configured map[string]bool) (int, error) {
	tasks, err := s.getTasks(ctx)
	if err != nil {
		return 0, err
	}
	newest := map[string]taskItem{}
	ids := map[string]syncWorkerID{}
	for _, t := range tasks {
		if t.WorkerType != "syncjob" || !t.finished() {
			continue
		}
		id, ok := parseSyncWorkerID(t.WorkerID)
		if !ok || configured[id.jobID] {
			continue
		}
		if prev, seen := newest[id.jobID]; !seen || t.StartTime > prev.StartTime {
			newest[id.jobID] = t
			ids[id.jobID] = id
		}
	}
	for jobID, t := range newest {
		id := ids[jobID]
		run := jobRun{state: t.Status, start: t.StartTime, end: t.EndTime, duration: -1}
		collectJobRun(ch, run, "sync", jobID, id.store, id.ns, "tasks")
		ch <- prometheus.MustNewConstMetric(syncJobInfo, prometheus.GaugeValue, 1,
			jobID, id.store, id.ns, "tasks", id.remote, id.remoteStore, "", "", "", "", "", "")
	}
	return len(newest), nil
}
