package main

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// jobLabels identify a job. source is "config" for jobs read from the job
// endpoints and "tasks" for sync jobs derived from the task log.
var jobLabels = []string{"job_type", "job_id", "datastore", "pbs_namespace", "source"}

func jobDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(promNamespace, "job", name), help, jobLabels, nil)
}

var (
	jobLastRunSuccess  = jobDesc("last_run_success", "Whether the last run of the job ended without error (OK or with warnings).")
	jobLastRunEnd      = jobDesc("last_run_end_timestamp_seconds", "End of the last run of the job.")
	jobNextRun         = jobDesc("next_run_timestamp_seconds", "Next scheduled run of the job.")
	jobInterval        = jobDesc("interval_seconds", "Time between the start of the last run and the next run of the job.")
	jobLastRunDuration = jobDesc("last_run_duration_seconds", "Duration of the last run of the job.")

	verifyJobInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "verify_job", "info"),
		"Settings of a verify job, always 1.",
		[]string{"job_id", "datastore", "pbs_namespace", "outdated_after", "ignore_verified", "max_depth", "schedule"}, nil,
	)
	pruneJobKeep = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "prune_job", "keep"),
		"Retention setting of a prune job.",
		[]string{"job_id", "datastore", "pbs_namespace", "keep"}, nil,
	)
)

// jobStatus is the status part of a job entry.
type jobStatus struct {
	LastRunState string `json:"last-run-state"`
	LastRunEnd   int64  `json:"last-run-endtime"`
	LastRunUPID  string `json:"last-run-upid"`
	NextRun      int64  `json:"next-run"`
}

// jobRun is what is known about the last and the next run of a job; zero
// values are unknown.
type jobRun struct {
	state      string
	start, end int64
	next       int64
	// duration is -1 when it has to be derived from start and end
	duration int64
	// workerType and workerID identify the tasks of a running job, to find
	// its previous run in the task log
	workerType, workerID string
}

func (st jobStatus) run() jobRun {
	start, _ := parseUPIDStart(st.LastRunUPID)
	run := jobRun{state: st.LastRunState, start: start, end: st.LastRunEnd, next: st.NextRun, duration: -1}
	// while the job runs, PBS reports the running task without state
	if st.LastRunState == "" {
		run.workerType, run.workerID, _ = parseUPIDWorker(st.LastRunUPID)
	}
	return run
}

// withPreviousRun completes the run of a job that is running right now: PBS
// then reports no state and end, so they are taken from the newest finished
// task of the job in the task log. The interval stays measured from the start
// of the running task.
func (s *scrapeState) withPreviousRun(ctx context.Context, run jobRun) (jobRun, error) {
	if run.state != "" || run.workerType == "" {
		return run, nil
	}
	tasks, err := s.getTasks(ctx)
	if !usableTasks(err) {
		return run, err
	}
	prev, ok := newestPerKey(tasks, func(t taskItem) (struct{}, bool) {
		return struct{}{}, t.WorkerType == run.workerType && t.WorkerID == run.workerID
	})[struct{}{}]
	if ok {
		run.state, run.end, run.duration = prev.Status, prev.EndTime, max(prev.EndTime-prev.StartTime, 0)
	}
	return run, err
}

// collectJobRun exports the run metrics of a job. Values PBS does not report
// are omitted.
func collectJobRun(ch chan<- prometheus.Metric, run jobRun, labels ...string) {
	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	if run.state != "" {
		success := 0.0
		if succeeded(run.state) {
			success = 1
		}
		gauge(jobLastRunSuccess, success)
	}
	if run.end > 0 {
		gauge(jobLastRunEnd, float64(run.end))
	}
	if run.next > 0 {
		gauge(jobNextRun, float64(run.next))
	}
	if run.start > 0 && run.next > run.start {
		gauge(jobInterval, float64(run.next-run.start))
	}
	duration := run.duration
	if duration < 0 && run.start > 0 && run.end >= run.start {
		duration = run.end - run.start
	}
	if duration >= 0 {
		gauge(jobLastRunDuration, float64(duration))
	}
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
	keepOptions
	jobStatus
}

type gcJob struct {
	Store        string `json:"store"`
	LastRunState string `json:"last-run-state"`
	LastRunEnd   int64  `json:"last-run-endtime"`
	NextRun      int64  `json:"next-run"`
	Duration     *int64 `json:"duration"`
	UPID         string `json:"upid"`
}

var jobsCollector = &collector{
	name:      "jobs",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs: []*prometheus.Desc{
		jobLastRunSuccess, jobLastRunEnd, jobNextRun, jobInterval, jobLastRunDuration,
		verifyJobInfo, pruneJobKeep,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return errors.Join(
			collectVerifyJobs(ctx, s, ch),
			collectPruneJobs(ctx, s, ch),
			collectGCJobs(ctx, s, ch),
		)
	},
}

func collectVerifyJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []verifyJob
	if err := s.api.get(ctx, apiPath("admin", "verify"), nil, &jobs); err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		run, err := s.withPreviousRun(ctx, j.run())
		errs = append(errs, err)
		collectJobRun(ch, run, "verify", j.ID, j.Store, j.Namespace, "config")
		ch <- prometheus.MustNewConstMetric(verifyJobInfo, prometheus.GaugeValue, 1,
			j.ID, j.Store, j.Namespace, optInt(j.OutdatedAfter), optBool(j.IgnoreVerified), optInt(j.MaxDepth), j.Schedule)
	}
	return joinUnique(errs...)
}

func collectPruneJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []pruneJob
	if err := s.api.get(ctx, apiPath("admin", "prune"), nil, &jobs); err != nil {
		return err
	}
	// the tasks of a prune job are named after its datastore and namespace,
	// not after the job, so they tell jobs on the same namespace apart
	type namespaceKey struct{ store, ns string }
	jobsPerNamespace := map[namespaceKey]int{}
	for _, j := range jobs {
		jobsPerNamespace[namespaceKey{j.Store, j.Namespace}]++
	}
	var errs []error
	for _, j := range jobs {
		run := j.run()
		if jobsPerNamespace[namespaceKey{j.Store, j.Namespace}] > 1 {
			// a running job's previous run could be another job's run
			run.workerType = ""
		}
		run, err := s.withPreviousRun(ctx, run)
		errs = append(errs, err)
		collectJobRun(ch, run, "prune", j.ID, j.Store, j.Namespace, "config")
		for keep, v := range j.set() {
			ch <- prometheus.MustNewConstMetric(pruneJobKeep, prometheus.GaugeValue, float64(v), j.ID, j.Store, j.Namespace, keep)
		}
	}
	return joinUnique(errs...)
}

func collectGCJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []gcJob
	if err := s.api.get(ctx, apiPath("admin", "gc"), nil, &jobs); err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		run, err := s.gcRun(ctx, j)
		errs = append(errs, err)
		// GC runs per datastore, the store is its job ID
		collectJobRun(ch, run, "gc", j.Store, j.Store, "", "config")
	}
	return joinUnique(errs...)
}

// gcRun returns the last run of the garbage collection of a datastore. The
// GC status PBS reports (UPID, duration) is the one of the last successful
// run; the state and end time are the ones of the last run.
func (s *scrapeState) gcRun(ctx context.Context, j gcJob) (jobRun, error) {
	run := jobRun{state: j.LastRunState, end: j.LastRunEnd, next: j.NextRun, duration: -1}
	switch {
	case j.LastRunState == "":
		// running: the previous run is the newest finished GC task. Its
		// start is not the last start, and PBS computes the next run from
		// now, so there is no interval (start stays 0).
		run.workerType, run.workerID = "garbage_collection", j.Store
		return s.withPreviousRun(ctx, run)
	case succeeded(j.LastRunState):
		run.start, _ = parseUPIDStart(j.UPID)
		if j.Duration != nil {
			run.duration = *j.Duration
		}
		return run, nil
	}
	// failed: the start is the one of the GC task that ended at the end time
	tasks, err := s.getTasks(ctx)
	if !usableTasks(err) {
		return run, err
	}
	for _, t := range tasks {
		if t.WorkerType == "garbage_collection" && t.WorkerID == j.Store && t.EndTime == j.LastRunEnd {
			run.start = t.StartTime
		}
	}
	return run, err
}
