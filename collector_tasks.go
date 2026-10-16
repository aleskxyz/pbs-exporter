package main

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

var tasksRunning = prometheus.NewDesc(
	prometheus.BuildFQName(promNamespace, "", "tasks_running"),
	"Number of running tasks.",
	[]string{"worker_type"}, nil,
)

// taskItem is an entry of /nodes/localhost/tasks.
type taskItem struct {
	UPID       string `json:"upid"`
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

// getTasks returns the tasks of the last task_lookback, at most task_limit.
// The task log is read once per scrape.
func (s *scrapeState) getTasks(ctx context.Context) ([]taskItem, error) {
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
	descs:     []*prometheus.Desc{tasksRunning},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
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
		return nil
	},
}
