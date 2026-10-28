package main

import (
	"context"
	"errors"
	"net/url"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	userInfo = newDesc("user", "info",
		"A user of the PBS, always 1. enabled is the enable setting as PBS reports it, empty when not set (PBS treats that as enabled).",
		"userid", "enabled")
	userExpiry = newDesc("user", "expiry_timestamp_seconds",
		"The expire setting of the user account as PBS reports it (0 = never); omitted when not set.",
		"userid")
	tokenExpiry = newDesc("token", "expiry_timestamp_seconds",
		"The expire setting of the API token as PBS reports it (0 = never); omitted when not set.",
		"tokenid")
)

// userConfig is an entry of /access/users. Comments are not read: they are
// free text and sometimes hold secrets.
type userConfig struct {
	UserID string `json:"userid"`
	Enable *bool  `json:"enable"`
	Expire *int64 `json:"expire"`
	Tokens []struct {
		TokenID string `json:"tokenid"`
		Expire  *int64 `json:"expire"`
	} `json:"tokens"`
}

// errUsersFiltered is returned when the token lacks Sys.Audit on
// /access/users: PBS then lists only the token's own user, without an error.
var errUsersFiltered = errors.New("missing privilege Sys.Audit on /access/users: only the user of the API token is listed")

var accessCollector = &collector{
	name:      "access",
	enabled:   false,
	privilege: "Sys.Audit on /access/users",
	descs:     []*prometheus.Desc{userInfo, userExpiry, tokenExpiry},
	collect:   collectUsers,
}

func collectUsers(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var users []userConfig
	if err := s.api.get(ctx, apiPath("access", "users"), url.Values{"include_tokens": {"1"}}, &users); err != nil {
		return err
	}
	for _, u := range users {
		gauge(ch, userInfo, 1, u.UserID, optBool(u.Enable))
		optGauge(ch, userExpiry, u.Expire, u.UserID)
		for _, t := range u.Tokens {
			optGauge(ch, tokenExpiry, t.Expire, t.TokenID)
		}
	}
	return s.privilegeGap(ctx, permissions.canAuditUsers, errUsersFiltered)
}
