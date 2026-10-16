package main

import (
	"context"
	"errors"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// jobLabels identify a job. source is "config" for jobs read from the job
// endpoints and "tasks" for sync jobs derived from the task log.
var jobLabels = []string{"job_type", "job_id", "store", "pbs_namespace", "source"}

func jobDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(promNamespace, "job", name), help, jobLabels, nil)
}

var (
	jobLastRunSuccess  = jobDesc("last_run_success", "Whether the last run of the job ended OK.")
	jobLastRunEnd      = jobDesc("last_run_end_timestamp_seconds", "End of the last run of the job.")
	jobNextRun         = jobDesc("next_run_timestamp_seconds", "Next scheduled run of the job.")
	jobInterval        = jobDesc("interval_seconds", "Time between the start of the last run and the next run of the job.")
	jobLastRunDuration = jobDesc("last_run_duration_seconds", "Duration of the last run of the job.")

	verifyJobInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "verify_job", "info"),
		"Settings of a verify job, always 1.",
		[]string{"job_id", "store", "pbs_namespace", "outdated_after", "ignore_verified", "max_depth", "schedule"}, nil,
	)
	pruneJobKeep = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "prune_job", "keep"),
		"Retention setting of a prune job.",
		[]string{"job_id", "store", "pbs_namespace", "keep"}, nil,
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
	if err != nil {
		return run, err
	}
	var prev *taskItem
	for i, t := range tasks {
		if t.WorkerType == run.workerType && t.WorkerID == run.workerID && t.finished() &&
			(prev == nil || t.StartTime > prev.StartTime) {
			prev = &tasks[i]
		}
	}
	if prev != nil {
		run.state, run.end, run.duration = prev.Status, prev.EndTime, prev.EndTime-prev.StartTime
	}
	return run, nil
}

// collectJobRun exports the run metrics of a job. Values PBS does not report
// are omitted.
func collectJobRun(ch chan<- prometheus.Metric, run jobRun, labels ...string) {
	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	if run.state != "" {
		success := 0.0
		if run.state == "OK" {
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
	if run.next > 0 && run.start > 0 && run.next > run.start {
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

func optInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

func optBool(v *bool) string {
	if v == nil {
		return ""
	}
	return strconv.FormatBool(*v)
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

// keepOptions are the retention settings of prune jobs and datastores.
type keepOptions struct {
	KeepLast    *int64 `json:"keep-last"`
	KeepHourly  *int64 `json:"keep-hourly"`
	KeepDaily   *int64 `json:"keep-daily"`
	KeepWeekly  *int64 `json:"keep-weekly"`
	KeepMonthly *int64 `json:"keep-monthly"`
	KeepYearly  *int64 `json:"keep-yearly"`
}

// set returns the retention settings that are set, keyed by the keep label.
func (k keepOptions) set() map[string]int64 {
	out := map[string]int64{}
	for name, v := range map[string]*int64{
		"last": k.KeepLast, "hourly": k.KeepHourly, "daily": k.KeepDaily,
		"weekly": k.KeepWeekly, "monthly": k.KeepMonthly, "yearly": k.KeepYearly,
	} {
		if v != nil {
			out[name] = *v
		}
	}
	return out
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
	return errors.Join(errs...)
}

func collectPruneJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []pruneJob
	if err := s.api.get(ctx, apiPath("admin", "prune"), nil, &jobs); err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		run, err := s.withPreviousRun(ctx, j.run())
		errs = append(errs, err)
		collectJobRun(ch, run, "prune", j.ID, j.Store, j.Namespace, "config")
		for keep, v := range j.set() {
			ch <- prometheus.MustNewConstMetric(pruneJobKeep, prometheus.GaugeValue, float64(v), j.ID, j.Store, j.Namespace, keep)
		}
	}
	return errors.Join(errs...)
}

func collectGCJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []gcJob
	if err := s.api.get(ctx, apiPath("admin", "gc"), nil, &jobs); err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		start, _ := parseUPIDStart(j.UPID)
		run := jobRun{state: j.LastRunState, start: start, end: j.LastRunEnd, next: j.NextRun, duration: -1}
		if j.Duration != nil {
			run.duration = *j.Duration
		}
		if j.LastRunState == "" {
			// running: the previous run is the newest finished GC task
			run.workerType, run.workerID, run.duration = "garbage_collection", j.Store, -1
		}
		run, err := s.withPreviousRun(ctx, run)
		errs = append(errs, err)
		// GC runs per datastore, the store is its job ID
		collectJobRun(ch, run, "gc", j.Store, j.Store, "", "config")
	}
	return errors.Join(errs...)
}
