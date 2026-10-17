package main

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	tasksRunning = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "tasks_running"),
		"Number of running tasks.",
		[]string{"worker_type"}, nil,
	)
	taskFailures = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "task_failures"),
		"Tasks that ended with an error in the last 24 hours, or task_lookback if shorter (warnings do not count); omitted when there are none.",
		[]string{"worker_type", "datastore"}, nil,
	)
)

// taskFailureWindow is how far back pbs_task_failures counts.
const taskFailureWindow = 24 * time.Hour

// taskFailed reports whether a finished task ended with an error.
func taskFailed(t taskItem) bool {
	return t.finished() && !succeeded(t.Status)
}

// taskItem is an entry of /nodes/localhost/tasks.
type taskItem struct {
	WorkerType string `json:"worker_type"`
	WorkerID   string `json:"worker_id"`
	User       string `json:"user"`
	StartTime  int64  `json:"starttime"`
	// EndTime and Status are not set while the task is running.
	EndTime int64  `json:"endtime"`
	Status  string `json:"status"`
}

func (t taskItem) finished() bool {
	return t.EndTime > 0 && t.Status != ""
}

// newestPerKey returns the newest finished task per key; key reports false
// for tasks to leave out.
func newestPerKey[K comparable](tasks []taskItem, key func(taskItem) (K, bool)) map[K]taskItem {
	out := map[K]taskItem{}
	for _, t := range tasks {
		if !t.finished() {
			continue
		}
		k, ok := key(t)
		if !ok {
			continue
		}
		if prev, seen := out[k]; !seen || t.StartTime > prev.StartTime {
			out[k] = t
		}
	}
	return out
}

// countTasks counts the tasks per key; key reports false for tasks to leave
// out.
func countTasks[K comparable](tasks []taskItem, key func(taskItem) (K, bool)) map[K]int {
	counts := map[K]int{}
	for _, t := range tasks {
		if k, ok := key(t); ok {
			counts[k]++
		}
	}
	return counts
}

// succeeded reports whether a task or job run ended without error; a run
// with warnings ("WARNINGS: n") ended, as PBS treats it, successfully.
func succeeded(status string) bool {
	return status == "OK" || strings.HasPrefix(status, "WARNINGS")
}

// errTaskLogFiltered is returned with the task log when the token lacks
// Sys.Audit on /system/tasks: PBS then lists only the token's own tasks,
// without an error.
var errTaskLogFiltered = errors.New("missing privilege Sys.Audit on /system/tasks: the task log only shows the tasks of the API token, metrics taken from it are incomplete")

// permissions are the privileges of the token per ACL path, as listed by
// /access/permissions; the value tells whether a privilege propagates to the
// paths below.
type permissions map[string]map[string]bool

// getPermissions returns the privileges of the token, read once per scrape.
func (s *scrapeState) getPermissions(ctx context.Context) (permissions, error) {
	return s.permissions.get(struct{}{}, func() (permissions, error) {
		var out permissions
		err := s.api.get(ctx, apiPath("access", "permissions"), nil, &out)
		return out, err
	})
}

// canAuditTasks reports whether the token sees all tasks: Sys.Audit on
// /system/tasks itself, or propagated from /system (or "/").
func (p permissions) canAuditTasks() bool {
	if _, ok := p["/system/tasks"]["Sys.Audit"]; ok {
		return true
	}
	return p["/system"]["Sys.Audit"]
}

// taskLogFiltered returns errTaskLogFiltered if the task log of the token is
// incomplete. If the permissions can not be read, that is not known.
func (s *scrapeState) taskLogFiltered(ctx context.Context) error {
	if perms, err := s.getPermissions(ctx); err == nil && !perms.canAuditTasks() {
		return errTaskLogFiltered
	}
	return nil
}

// getTasks returns the tasks of the last task_lookback, at most task_limit.
// The task log is read once per scrape. With errTaskLogFiltered, the tasks
// are returned as well, but are incomplete.
func (s *scrapeState) getTasks(ctx context.Context) ([]taskItem, error) {
	tasks, err := s.getAllTasks(ctx)
	if err != nil {
		return nil, err
	}
	return tasks, s.taskLogFiltered(ctx)
}

// usableTasks reports whether getTasks returned tasks to work with.
func usableTasks(err error) bool {
	return err == nil || errors.Is(err, errTaskLogFiltered)
}

func (s *scrapeState) getAllTasks(ctx context.Context) ([]taskItem, error) {
	return s.tasks.get(struct{}{}, func() ([]taskItem, error) {
		q := url.Values{
			"since": {strconv.FormatInt(s.now.Add(-s.module.taskLookback).Unix(), 10)},
			"limit": {strconv.Itoa(s.module.taskLimit)},
		}
		var out []taskItem
		if err := s.api.get(ctx, apiPath("nodes", "localhost", "tasks"), q, &out); err != nil {
			return nil, err
		}
		if len(out) >= s.module.taskLimit {
			slog.Warn("the task log has more tasks than task_limit, older tasks are not seen",
				"module", sanitize(s.module.name), "task_limit", s.module.taskLimit, "task_lookback", s.module.taskLookback)
		}
		return out, nil
	})
}

var tasksCollector = &collector{
	name:      "tasks",
	enabled:   true,
	privilege: "Sys.Audit on /system/tasks",
	descs:     []*prometheus.Desc{tasksRunning, taskFailures},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return joinUnique(collectRunningTasks(ctx, s, ch), collectTaskFailures(ctx, s, ch))
	},
}

func collectTaskFailures(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	tasks, err := s.getTasks(ctx)
	if !usableTasks(err) {
		return err
	}
	type key struct{ workerType, store string }
	since := s.now.Add(-taskFailureWindow).Unix()
	counts := countTasks(tasks, func(t taskItem) (key, bool) {
		return key{t.WorkerType, taskStore(t)}, taskFailed(t) && t.EndTime >= since
	})
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(taskFailures, prometheus.GaugeValue, float64(n), k.workerType, k.store)
	}
	return err
}

func collectRunningTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	q := url.Values{"running": {"1"}, "limit": {strconv.Itoa(s.module.taskLimit)}}
	var running []taskItem
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "tasks"), q, &running); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, t := range running {
		counts[t.WorkerType]++
	}
	for workerType, n := range counts {
		ch <- prometheus.MustNewConstMetric(tasksRunning, prometheus.GaugeValue, float64(n), workerType)
	}
	return s.taskLogFiltered(ctx)
}
