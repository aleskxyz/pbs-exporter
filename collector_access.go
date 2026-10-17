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
		"Reader tasks (restores, and syncs from other servers) started in the last 24 hours, or task_lookback if shorter, per user or API token.",
		[]string{"userid"}, nil,
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
		Enable  *bool  `json:"enable"`
		Expire  *int64 `json:"expire"`
	} `json:"tokens"`
}

// enabled reports whether an account is enabled; PBS leaves out "enable"
// for the default, enabled.
func enabled(enable *bool) bool {
	return enable == nil || *enable
}

// errUsersFiltered is returned when the token lacks Sys.Audit on
// /access/users: PBS then lists only the token's own user, without an error.
var errUsersFiltered = errors.New("missing privilege Sys.Audit on /access/users: only the user of the API token is listed")

// canAuditUsers reports whether the token sees all users: Sys.Audit on
// /access/users itself, or propagated from /access (or "/").
func (p permissions) canAuditUsers() bool {
	if _, ok := p["/access/users"]["Sys.Audit"]; ok {
		return true
	}
	return p["/access"]["Sys.Audit"]
}

var accessCollector = &collector{
	name:      "access",
	enabled:   false,
	privilege: "Sys.Audit on /access/users and /system/tasks",
	descs:     []*prometheus.Desc{userInfo, userExpiry, tokenExpiry, readerTasks},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return joinUnique(collectUsers(ctx, s, ch), collectReaderTasks(ctx, s, ch))
	},
}

func collectUsers(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var users []userConfig
	if err := s.api.get(ctx, apiPath("access", "users"), url.Values{"include_tokens": {"1"}}, &users); err != nil {
		return err
	}
	for _, u := range users {
		userEnabled := enabled(u.Enable)
		ch <- prometheus.MustNewConstMetric(userInfo, prometheus.GaugeValue, 1, u.UserID, strconv.FormatBool(userEnabled))
		// the expiry of a disabled account (or of a token of a disabled
		// user) does not matter
		if !userEnabled {
			continue
		}
		if u.Expire != nil && *u.Expire > 0 {
			ch <- prometheus.MustNewConstMetric(userExpiry, prometheus.GaugeValue, float64(*u.Expire), u.UserID)
		}
		for _, t := range u.Tokens {
			if enabled(t.Enable) && t.Expire != nil && *t.Expire > 0 {
				ch <- prometheus.MustNewConstMetric(tokenExpiry, prometheus.GaugeValue, float64(*t.Expire), t.TokenID)
			}
		}
	}
	if perms, err := s.getPermissions(ctx); err == nil && !perms.canAuditUsers() {
		return errUsersFiltered
	}
	return nil
}

func collectReaderTasks(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	tasks, err := s.getTasks(ctx)
	if !usableTasks(err) {
		return err
	}
	since := s.now.Add(-readerTaskWindow).Unix()
	counts := countTasks(tasks, func(t taskItem) (string, bool) {
		return t.User, t.WorkerType == "reader" && t.StartTime >= since
	})
	for user, n := range counts {
		ch <- prometheus.MustNewConstMetric(readerTasks, prometheus.GaugeValue, float64(n), user)
	}
	return err
}
