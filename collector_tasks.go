package main

import (
	"context"
	"errors"
	"net/url"

	"github.com/prometheus/client_golang/prometheus"
)

var tasksRunning = newDesc("", "tasks_running",
	"Number of running tasks.",
	"worker_type")

// taskItem is an entry of /nodes/localhost/tasks.
type taskItem struct {
	WorkerType string `json:"worker_type"`
}

// errTaskLogFiltered is returned when the token lacks Sys.Audit on
// /system/tasks: PBS then lists only the token's own tasks, without an error.
var errTaskLogFiltered = errors.New("missing privilege Sys.Audit on /system/tasks: the task list only shows the tasks of the API token")

var tasksCollector = &collector{
	name:      "tasks",
	enabled:   true,
	privilege: "Sys.Audit on /system/tasks",
	descs:     []*prometheus.Desc{tasksRunning},
	collect:   collectRunningTasks,
}

func collectRunningTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	// limit 0 lists all running tasks; PBS lists 50 by default
	q := url.Values{"running": {"1"}, "limit": {"0"}}
	var running []taskItem
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "tasks"), q, &running); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, t := range running {
		counts[t.WorkerType]++
	}
	for workerType, n := range counts {
		gauge(ch, tasksRunning, float64(n), workerType)
	}
	return s.privilegeGap(ctx, permissions.canAuditTasks, errTaskLogFiltered)
}
