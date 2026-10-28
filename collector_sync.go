package main

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	syncJobInfo = newDesc("sync_job", "info",
		"Settings of a sync job, always 1. datastore and datastore_namespace are local, remote_* the other side; sync_direction (empty when PBS reports none, which PBS treats as pull) tells which side is the source.",
		"job_id", "datastore", "datastore_namespace", "remote", "remote_store", "remote_ns",
		"sync_direction", "max_depth", "remove_vanished", "transfer_last", "group_filter",
		"verified_only", "encrypted_only", "schedule")
	remoteInfo = newDesc("remote", "info",
		"A remote PBS that sync jobs read from or write to, always 1. fingerprint is the pinned certificate fingerprint (lower case), empty when the remote's certificate is trusted otherwise.",
		"remote", "host", "port", "fingerprint", "auth_id")
)

type syncJob struct {
	ID             string   `json:"id"`
	Store          string   `json:"store"`
	Namespace      string   `json:"ns"`
	Remote         string   `json:"remote"`
	RemoteStore    string   `json:"remote-store"`
	RemoteNS       string   `json:"remote-ns"`
	SyncDirection  string   `json:"sync-direction"`
	MaxDepth       *int64   `json:"max-depth"`
	RemoveVanished *bool    `json:"remove-vanished"`
	TransferLast   *int64   `json:"transfer-last"`
	GroupFilter    []string `json:"group-filter"`
	VerifiedOnly   *bool    `json:"verified-only"`
	EncryptedOnly  *bool    `json:"encrypted-only"`
	Schedule       string   `json:"schedule"`
	jobStatus
}

type remoteConfig struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        *int64 `json:"port"`
	Fingerprint string `json:"fingerprint"`
	AuthID      string `json:"auth-id"`
}

// errSyncJobsFiltered is returned when the token lacks Remote.Audit on
// /remote: PBS then lists only the sync jobs and remotes of the remotes the
// token may audit, without an error.
var errSyncJobsFiltered = errors.New("missing privilege Remote.Audit on /remote: the sync jobs and remotes of remotes the token can not audit are not listed")

var syncCollector = &collector{
	name:      "sync",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore, Remote.Audit on /remote",
	descs:     append(append([]*prometheus.Desc(nil), jobDescs...), syncJobInfo, remoteInfo),
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		// PBS leaves out the jobs and remotes the token may not see, without
		// an error: the jobs of a remote need Remote.Audit on that remote
		// and only the jobs of the local datastores the token can audit
		return joinUnique(
			collectSyncJobs(ctx, s, ch),
			collectRemotes(ctx, s, ch),
			s.privilegeGap(ctx, permissions.hasRemoteAudit, errSyncJobsFiltered),
			s.privilegeGap(ctx, permissions.canAuditDatastores, errDatastoresFiltered),
		)
	},
}

// collectSyncJobs exports the sync jobs PBS lists.
func collectSyncJobs(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var jobs []syncJob
	if err := s.api.get(ctx, apiPath("admin", "sync"), url.Values{"sync-direction": {"all"}}, &jobs); err != nil {
		return err
	}
	for _, j := range jobs {
		collectJobRun(ch, j.run(), "sync", j.ID, j.Store, j.Namespace)
		filter := slices.Sorted(slices.Values(j.GroupFilter))
		gauge(ch, syncJobInfo, 1,
			j.ID, j.Store, j.Namespace, j.Remote, j.RemoteStore, j.RemoteNS,
			j.SyncDirection, optInt(j.MaxDepth), optBool(j.RemoveVanished), optInt(j.TransferLast), strings.Join(filter, ","),
			optBool(j.VerifiedOnly), optBool(j.EncryptedOnly), j.Schedule)
	}
	return nil
}

func collectRemotes(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var remotes []remoteConfig
	if err := s.api.get(ctx, apiPath("config", "remote"), nil, &remotes); err != nil {
		return err
	}
	for _, r := range remotes {
		gauge(ch, remoteInfo, 1,
			r.Name, r.Host, optInt(r.Port), normalizeFingerprint(r.Fingerprint), r.AuthID)
	}
	return nil
}
