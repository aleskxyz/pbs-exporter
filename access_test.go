package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Users and API tokens are exported with their enable and expire settings as
// PBS reports them; their comments, which sometimes hold secrets, are not.
// The collector is off by default.
func TestAccess(t *testing.T) {
	srv := newFakePBS(t, nil)
	if n := testutil.CollectAndCount(testExporter(t, srv, ModuleConfig{}), "pbs_user_info"); n != 0 {
		t.Errorf("access must be off by default, got %d series", n)
	}

	e := testExporter(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true}})
	compare(t, e, `
# HELP pbs_user_info A user of the PBS, always 1. enabled is the enable setting as PBS reports it, empty when not set (PBS treats that as enabled).
# TYPE pbs_user_info gauge
pbs_user_info{enabled="true",userid="mon@pbs"} 1
pbs_user_info{enabled="false",userid="old@pbs"} 1
pbs_user_info{enabled="",userid="root@pam"} 1
# HELP pbs_user_expiry_timestamp_seconds The expire setting of the user account as PBS reports it (0 = never); omitted when not set.
# TYPE pbs_user_expiry_timestamp_seconds gauge
pbs_user_expiry_timestamp_seconds{userid="mon@pbs"} 1.9e+09
pbs_user_expiry_timestamp_seconds{userid="old@pbs"} 1.6e+09
# HELP pbs_token_expiry_timestamp_seconds The expire setting of the API token as PBS reports it (0 = never); omitted when not set.
# TYPE pbs_token_expiry_timestamp_seconds gauge
pbs_token_expiry_timestamp_seconds{tokenid="mon@pbs!disabled"} 1.7e+09
pbs_token_expiry_timestamp_seconds{tokenid="mon@pbs!exporter"} 1.8e+09
pbs_token_expiry_timestamp_seconds{tokenid="mon@pbs!old"} 0
pbs_token_expiry_timestamp_seconds{tokenid="old@pbs!t"} 1.65e+09
`, "pbs_user_info", "pbs_user_expiry_timestamp_seconds", "pbs_token_expiry_timestamp_seconds")

	h := newMetricsHandler(map[string]*Module{"site": testModule(t, srv, ModuleConfig{Collectors: map[string]bool{"access": true}})}, testTimeouts)
	code, body := scrape(t, h, "module=site")
	if code != http.StatusOK || strings.Contains(body, "hunter2") || strings.Contains(body, "token secret") {
		t.Errorf("status %d, or a comment was exported:\n%s", code, body)
	}
}
