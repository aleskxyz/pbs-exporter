package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/prometheus/client_golang/prometheus"
)

var snapshotCount = prometheus.NewDesc(
	prometheus.BuildFQName(promNamespace, "", "snapshot_count"),
	"The total number of backups of a namespace.",
	[]string{"datastore", "pbs_namespace", "namespace_root"}, nil,
)

type namespaceItem struct {
	Namespace string `json:"ns"`
}

// snapshotItem is an entry of /admin/datastore/{store}/snapshots.
type snapshotItem struct {
	BackupType string `json:"backup-type"`
	BackupID   string `json:"backup-id"`
	BackupTime int64  `json:"backup-time"`
	Comment    string `json:"comment"`
	// Size is not set while the snapshot has no manifest: the backup is
	// still running (or the manifest is not readable).
	Size         *int64 `json:"size"`
	Verification struct {
		State string `json:"state"`
	} `json:"verification"`
}

func (s *scrapeState) getNamespaces(ctx context.Context, store string) ([]namespaceItem, error) {
	return s.namespaces.get(store, func() ([]namespaceItem, error) {
		var out []namespaceItem
		err := s.api.get(ctx, apiPath("admin", "datastore", store, "namespace"), nil, &out)
		return out, err
	})
}

func (s *scrapeState) getSnapshots(ctx context.Context, store, ns string) ([]snapshotItem, error) {
	return s.snapshots.get(storeNamespace{store, ns}, func() ([]snapshotItem, error) {
		var out []snapshotItem
		err := s.api.get(ctx, apiPath("admin", "datastore", store, "snapshots"), url.Values{"ns": {ns}}, &out)
		return out, err
	})
}

// forEachNamespace calls f for every namespace of every readable datastore,
// concurrently. Datastores that turn out to be unavailable are skipped.
func (s *scrapeState) forEachNamespace(ctx context.Context, f func(store, ns string) error) error {
	stores, storesErr := s.readableDatastores(ctx)
	err := forEach(stores, s.module.maxConcurrency, func(store string) error {
		namespaces, err := s.getNamespaces(ctx, store)
		if isDatastoreUnavailable(err) {
			slog.Info("skipping unavailable datastore", "datastore", sanitize(store), "err", sanitize(err.Error()))
			return nil
		}
		if err != nil {
			return err
		}
		return forEach(namespaces, s.module.maxConcurrency, func(ns namespaceItem) error {
			err := f(store, ns.Namespace)
			// the datastore may become unavailable after its namespaces were listed
			if isDatastoreUnavailable(err) {
				slog.Info("skipping unavailable datastore", "datastore", sanitize(store), "err", sanitize(err.Error()))
				return nil
			}
			if err != nil {
				return fmt.Errorf("datastore %s, namespace %q: %w", store, ns.Namespace, err)
			}
			return nil
		})
	})
	return errors.Join(storesErr, err)
}

var snapshotsCollector = &collector{
	name:      "snapshots",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs:     []*prometheus.Desc{snapshotCount},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return s.forEachNamespace(ctx, func(store, ns string) error {
			snapshots, err := s.getSnapshots(ctx, store, ns)
			if err != nil {
				return err
			}
			ch <- prometheus.MustNewConstMetric(snapshotCount, prometheus.GaugeValue, float64(len(snapshots)), store, ns, namespaceRoot(ns))
			return nil
		})
	},
}
