package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// This file walks the content of the datastores: the readable datastores,
// their namespaces and the snapshots in them.

type namespaceItem struct {
	Namespace string `json:"ns"`
}

// snapshotItem is an entry of /admin/datastore/{store}/snapshots.
type snapshotItem struct {
	BackupType string `json:"backup-type"`
	BackupID   string `json:"backup-id"`
	BackupTime int64  `json:"backup-time"`
	// Size is not set while the snapshot has no manifest: the backup is
	// still running (or the manifest is not readable).
	Size         *int64 `json:"size"`
	Protected    bool   `json:"protected"`
	Verification struct {
		State string `json:"state"`
		UPID  string `json:"upid"`
	} `json:"verification"`
}

// finished reports whether a snapshot has a manifest; PBS lists running
// backups without one, and without notes, verification and size.
func (s snapshotItem) finished() bool {
	return s.Size != nil
}

func (s *scrapeState) getNamespaces(ctx context.Context, store string) ([]namespaceItem, error) {
	var out []namespaceItem
	err := s.api.get(ctx, apiPath("admin", "datastore", store, "namespace"), nil, &out)
	return out, err
}

func (s *scrapeState) getSnapshots(ctx context.Context, store, ns string) ([]snapshotItem, error) {
	var out []snapshotItem
	err := s.api.get(ctx, apiPath("admin", "datastore", store, "snapshots"), url.Values{"ns": {ns}}, &out)
	return out, err
}

// forEachNamespace calls f for every namespace of every readable datastore,
// concurrently. Every error is returned, also one of a datastore that became
// unavailable during the scrape.
func (s *scrapeState) forEachNamespace(ctx context.Context, f func(store, ns string) error) error {
	stores, err := s.readableDatastores(ctx)
	if err != nil {
		return err
	}
	return forEach(stores, s.maxConcurrency, func(store string) error {
		namespaces, err := s.getNamespaces(ctx, store)
		if err != nil {
			return fmt.Errorf("datastore %s: %w", store, err)
		}
		return forEach(namespaces, s.maxConcurrency, func(ns namespaceItem) error {
			if err := f(store, ns.Namespace); err != nil {
				return fmt.Errorf("datastore %s, namespace %q: %w", store, ns.Namespace, err)
			}
			return nil
		})
	})
}

// readableDatastores returns the datastores whose content can be read, as
// PBS reports them: not unmounted (/status/datastore-usage), not failed (PBS
// reports an error for them, see pbs_datastore_error) and not in a
// maintenance mode that blocks reading (/admin/datastore). Without both
// lists, which datastores are readable is unknown, and the error is returned.
func (s *scrapeState) readableDatastores(ctx context.Context) ([]string, error) {
	usage, err := s.getDatastoreUsage(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.getDatastoreList(ctx)
	if err != nil {
		return nil, err
	}
	modes := maintenanceModes(list)
	var stores []string
	for _, d := range usage {
		switch mode, inMaintenance := modes[d.Store]; {
		case d.MountStatus == "notmounted":
			slog.Debug("skipping unmounted datastore", "datastore", sanitize(d.Store))
		case inMaintenance && mode != "read-only":
			slog.Debug("skipping datastore in maintenance mode", "datastore", sanitize(d.Store), "mode", sanitize(mode))
		case d.Error != "":
			slog.Debug("skipping datastore PBS reports an error for", "datastore", sanitize(d.Store), "err", sanitize(d.Error))
		default:
			stores = append(stores, d.Store)
		}
	}
	return stores, nil
}

// maintenanceType returns the "type" of a maintenance mode property string
// ("offline", "read-only", ...); the key may be omitted ("offline,message=x"),
// and a quoted message may contain commas.
func maintenanceType(mode string) string {
	parts := splitPropertyString(mode)
	for _, part := range parts {
		if k, v, ok := strings.Cut(part, "="); ok && strings.TrimSpace(k) == "type" {
			return strings.TrimSpace(v)
		}
	}
	for _, part := range parts {
		if !strings.Contains(part, "=") {
			return strings.TrimSpace(part)
		}
	}
	return ""
}

// splitPropertyString splits a PBS property string at the commas that are
// not inside double quotes.
func splitPropertyString(s string) []string {
	var parts []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped character
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
