package main

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	userInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "user", "info"),
		"A user of the PBS, always 1.",
		[]string{"userid", "enabled"}, nil,
	)
	userExpiry = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "user", "expiry_timestamp_seconds"),
		"When the user account expires; omitted when it never expires.",
		[]string{"userid"}, nil,
	)
	tokenExpiry = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "token", "expiry_timestamp_seconds"),
		"When the API token expires; omitted when it never expires.",
		[]string{"tokenid"}, nil,
	)
	readerTasks = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "reader_tasks"),
		"Reader tasks (restores, and syncs from other servers) started in the last 24 hours, or task_lookback if shorter.",
		[]string{"user"}, nil,
	)
)

// readerTaskWindow is how far back pbs_reader_tasks counts.
const readerTaskWindow = taskFailureWindow

// userConfig is an entry of /access/users. Comments are not read: they are
// free text and sometimes hold secrets.
type userConfig struct {
	UserID string `json:"userid"`
	// Enable is not set for the default, enabled
	Enable *bool  `json:"enable"`
	Expire *int64 `json:"expire"`
	Tokens []struct {
		TokenID string `json:"tokenid"`
		Expire  *int64 `json:"expire"`
	} `json:"tokens"`
}

var accessCollector = &collector{
	name:      "access",
	enabled:   false,
	privilege: "Sys.Audit on /access",
	descs:     []*prometheus.Desc{userInfo, userExpiry, tokenExpiry, readerTasks},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return errors.Join(collectUsers(ctx, s, ch), collectReaderTasks(ctx, s, ch))
	},
}

func collectUsers(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var users []userConfig
	if err := s.api.get(ctx, apiPath("access", "users"), url.Values{"include_tokens": {"1"}}, &users); err != nil {
		return err
	}
	for _, u := range users {
		enabled := u.Enable == nil || *u.Enable
		ch <- prometheus.MustNewConstMetric(userInfo, prometheus.GaugeValue, 1, u.UserID, strconv.FormatBool(enabled))
		if u.Expire != nil && *u.Expire > 0 {
			ch <- prometheus.MustNewConstMetric(userExpiry, prometheus.GaugeValue, float64(*u.Expire), u.UserID)
		}
		for _, t := range u.Tokens {
			if t.Expire != nil && *t.Expire > 0 {
				ch <- prometheus.MustNewConstMetric(tokenExpiry, prometheus.GaugeValue, float64(*t.Expire), t.TokenID)
			}
		}
	}
	return nil
}

func collectReaderTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	tasks, err := s.getTasks(ctx)
	if err != nil {
		return err
	}
	since := s.now.Add(-readerTaskWindow).Unix()
	counts := map[string]int{}
	for _, t := range tasks {
		if t.WorkerType == "reader" && t.StartTime >= since {
			counts[t.User]++
		}
	}
	for user, n := range counts {
		ch <- prometheus.MustNewConstMetric(readerTasks, prometheus.GaugeValue, float64(n), user)
	}
	return nil
}
