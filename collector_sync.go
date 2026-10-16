package main

import (
	"context"
	"errors"
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

// errNoRemoteAudit is returned when the token can not read sync jobs and
// remotes. PBS does not refuse the calls, it answers with empty lists.
var errNoRemoteAudit = errors.New("missing privilege Remote.Audit on /remote: sync jobs and remotes are not readable, using the task log instead")

// hasPrivilege reports whether the permissions of /access/permissions grant
// priv on path or a path below it.
func hasPrivilege(perms map[string]map[string]any, path, priv string) bool {
	for p, privs := range perms {
		if p != path && !strings.HasPrefix(p, path+"/") {
			continue
		}
		if _, ok := privs[priv]; ok {
			return true
		}
	}
	return false
}

var syncCollector = &collector{
	name:      "sync",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore and Remote.Audit on /remote",
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
		if !hasPrivilege(perms, "/remote", "Remote.Audit") {
			return errors.Join(errNoRemoteAudit, collectSyncJobsFromTasks(ctx, s, ch))
		}
		return errors.Join(collectSyncJobs(ctx, s, ch), collectRemotes(ctx, s, ch))
	},
}

func collectSyncJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []syncJob
	if err := s.api.get(ctx, apiPath("admin", "sync"), url.Values{"sync-direction": {"all"}}, &jobs); err != nil {
		return err
	}
	for _, j := range jobs {
		direction := j.SyncDirection
		if direction == "" {
			direction = "pull"
		}
		collectJobRun(ch, j.run(), "sync", j.ID, j.Store, j.Namespace, "config")
		ch <- prometheus.MustNewConstMetric(syncJobInfo, prometheus.GaugeValue, 1,
			j.ID, j.Store, j.Namespace, "config", j.Remote, j.RemoteStore, j.RemoteNS,
			direction, optBool(j.RemoveVanished), optInt(j.TransferLast), strings.Join(j.GroupFilter, ","), j.Schedule)
	}
	return nil
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

// collectSyncJobsFromTasks derives the last run of each sync job from its
// newest syncjob task. The next run and the settings stay unknown, and a job
// whose last run is older than the task log is not seen.
func collectSyncJobsFromTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	tasks, err := s.getTasks(ctx)
	if err != nil {
		return err
	}
	newest := map[string]taskItem{}
	ids := map[string]syncWorkerID{}
	for _, t := range tasks {
		if t.WorkerType != "syncjob" || !t.finished() {
			continue
		}
		id, ok := parseSyncWorkerID(t.WorkerID)
		if !ok {
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
	return nil
}
