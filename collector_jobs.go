package main

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// jobLabels identify a job.
var jobLabels = []string{"job_type", "job_id", "datastore", "datastore_namespace"}

// The run metrics are the job state PBS reports: while a job runs, its last
// run is the running one, which has no state and no end yet.
var (
	jobLastRunSuccess = newDesc("job", "last_run_success",
		"Whether the last run of the job ended without error (last-run-state OK or with warnings); omitted while it runs.", jobLabels...)
	jobLastRunStart = newDesc("job", "last_run_start_timestamp_seconds",
		"Start of the last run of the job, running or finished (from last-run-upid; for GC only after a successful run).", jobLabels...)
	jobLastRunEnd = newDesc("job", "last_run_end_timestamp_seconds",
		"End of the last run of the job (last-run-endtime); omitted while it runs.", jobLabels...)
	jobNextRun = newDesc("job", "next_run_timestamp_seconds",
		"Next scheduled run of the job, as computed by PBS (next-run); in the past when a run is missed.", jobLabels...)

	verifyJobInfo = newDesc("verify_job", "info",
		"Settings of a verify job, always 1.",
		"job_id", "datastore", "datastore_namespace", "outdated_after", "ignore_verified", "max_depth", "schedule")
	pruneJobInfo = newDesc("prune_job", "info",
		"Settings of a prune job, always 1.",
		"job_id", "datastore", "datastore_namespace", "max_depth", "schedule", "disable")
	pruneJobKeep = newDesc("prune_job", "keep",
		"Retention setting of a prune job.",
		"job_id", "datastore", "datastore_namespace", "keep")
)

// jobDescs are the run metrics every job collector exports.
var jobDescs = []*prometheus.Desc{jobLastRunSuccess, jobLastRunStart, jobLastRunEnd, jobNextRun}

// jobStatus is the status part of a job entry.
type jobStatus struct {
	LastRunState string `json:"last-run-state"`
	LastRunEnd   *int64 `json:"last-run-endtime"`
	LastRunUPID  string `json:"last-run-upid"`
	NextRun      *int64 `json:"next-run"`
}

// succeeded reports whether a job run ended without error; a run with
// warnings ("WARNINGS: n") ended, as PBS treats it, successfully.
func succeeded(state string) bool {
	return state == "OK" || strings.HasPrefix(state, "WARNINGS")
}

// jobRun is the job state PBS reports; unset values are not reported.
type jobRun struct {
	state string
	// start is read from a UPID, 0 without one
	start     int64
	end, next *int64
}

func (st jobStatus) run() jobRun {
	start, _ := parseUPIDStart(st.LastRunUPID)
	return jobRun{state: st.LastRunState, start: start, end: st.LastRunEnd, next: st.NextRun}
}

// collectJobRun exports the run metrics of a job. Values PBS does not report
// are omitted.
func collectJobRun(ch chan<- prometheus.Metric, run jobRun, labels ...string) {
	if run.state != "" {
		success := 0.0
		if succeeded(run.state) {
			success = 1
		}
		gauge(ch, jobLastRunSuccess, success, labels...)
	}
	if run.start > 0 {
		gauge(ch, jobLastRunStart, float64(run.start), labels...)
	}
	optGauge(ch, jobLastRunEnd, run.end, labels...)
	optGauge(ch, jobNextRun, run.next, labels...)
}

type verifyJob struct {
	ID             string `json:"id"`
	Store          string `json:"store"`
	Namespace      string `json:"ns"`
	OutdatedAfter  *int64 `json:"outdated-after"`
	IgnoreVerified *bool  `json:"ignore-verified"`
	MaxDepth       *int64 `json:"max-depth"`
	Schedule       string `json:"schedule"`
	jobStatus
}

type pruneJob struct {
	ID        string `json:"id"`
	Store     string `json:"store"`
	Namespace string `json:"ns"`
	MaxDepth  *int64 `json:"max-depth"`
	Schedule  string `json:"schedule"`
	Disable   *bool  `json:"disable"`
	keepOptions
	jobStatus
}

type gcJob struct {
	Store string `json:"store"`
	jobStatus
	// UPID is the one of the last successful garbage collection; the GC
	// status has no last-run-upid
	UPID string `json:"upid"`
}

var jobsCollector = &collector{
	name:      "jobs",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs:     append(append([]*prometheus.Desc(nil), jobDescs...), verifyJobInfo, pruneJobInfo, pruneJobKeep),
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		// PBS lists only the jobs of the datastores the token can audit
		return joinUnique(
			collectVerifyJobs(ctx, s, ch),
			collectPruneJobs(ctx, s, ch),
			collectGCJobs(ctx, s, ch),
			s.privilegeGap(ctx, permissions.canAuditDatastores, errDatastoresFiltered),
		)
	},
}

func collectVerifyJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []verifyJob
	if err := s.api.get(ctx, apiPath("admin", "verify"), nil, &jobs); err != nil {
		return err
	}
	for _, j := range jobs {
		collectJobRun(ch, j.run(), "verify", j.ID, j.Store, j.Namespace)
		gauge(ch, verifyJobInfo, 1,
			j.ID, j.Store, j.Namespace, optInt(j.OutdatedAfter), optBool(j.IgnoreVerified), optInt(j.MaxDepth), j.Schedule)
	}
	return nil
}

func collectPruneJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []pruneJob
	if err := s.api.get(ctx, apiPath("admin", "prune"), nil, &jobs); err != nil {
		return err
	}
	for _, j := range jobs {
		collectJobRun(ch, j.run(), "prune", j.ID, j.Store, j.Namespace)
		gauge(ch, pruneJobInfo, 1,
			j.ID, j.Store, j.Namespace, optInt(j.MaxDepth), j.Schedule, optBool(j.Disable))
		for keep, v := range j.set() {
			gauge(ch, pruneJobKeep, float64(v), j.ID, j.Store, j.Namespace, keep)
		}
	}
	return nil
}

func collectGCJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []gcJob
	if err := s.api.get(ctx, apiPath("admin", "gc"), nil, &jobs); err != nil {
		return err
	}
	for _, j := range jobs {
		run := jobRun{state: j.LastRunState, end: j.LastRunEnd, next: j.NextRun}
		// the GC status names the last successful run, which is the last run
		// only when that succeeded
		if succeeded(j.LastRunState) {
			run.start, _ = parseUPIDStart(j.UPID)
		}
		// GC runs per datastore, the store is its job ID
		collectJobRun(ch, run, "gc", j.Store, j.Store, "")
	}
	return nil
}
