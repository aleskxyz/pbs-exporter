package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	snapshot_count = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "snapshot_count"),
		"The total number of backups.",
		[]string{"datastore", "namespace"}, nil,
	)
	snapshot_vm_count = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "snapshot_vm_count"),
		"The total number of backups per VM.",
		[]string{"datastore", "namespace", "vm_id", "vm_name"}, nil,
	)
	snapshot_vm_last_timestamp = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "snapshot_vm_last_timestamp"),
		"The timestamp of the last backup of a VM.",
		[]string{"datastore", "namespace", "vm_id", "vm_name"}, nil,
	)
	snapshot_vm_last_verify = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "snapshot_vm_last_verify"),
		"The verify status of the last backup of a VM.",
		[]string{"datastore", "namespace", "vm_id", "vm_name"}, nil,
	)
)

type namespaceItem struct {
	Namespace string `json:"ns"`
}

// snapshotItem is an entry of /admin/datastore/{store}/snapshots.
type snapshotItem struct {
	BackupType   string `json:"backup-type"`
	BackupID     string `json:"backup-id"`
	BackupTime   int64  `json:"backup-time"`
	Comment      string `json:"comment"`
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
	err := forEach(stores, s.maxConcurrency, func(store string) error {
		namespaces, err := s.getNamespaces(ctx, store)
		if isDatastoreUnavailable(err) {
			slog.Info("skipping unavailable datastore", "datastore", sanitize(store), "err", sanitize(err.Error()))
			return nil
		}
		if err != nil {
			return err
		}
		return forEach(namespaces, s.maxConcurrency, func(ns namespaceItem) error {
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
	descs:     []*prometheus.Desc{snapshot_count, snapshot_vm_count, snapshot_vm_last_timestamp, snapshot_vm_last_verify},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return s.forEachNamespace(ctx, func(store, ns string) error {
			snapshots, err := s.getSnapshots(ctx, store, ns)
			if err != nil {
				return err
			}
			return collectSnapshotMetrics(store, ns, snapshots, ch)
		})
	},
}

func collectSnapshotMetrics(datastore, namespace string, snapshots []snapshotItem, ch chan<- prometheus.Metric) error {
	ch <- prometheus.MustNewConstMetric(
		snapshot_count, prometheus.GaugeValue, float64(len(snapshots)), datastore, namespace,
	)

	// set snapshot metrics per vm
	vmNameMapping := make(map[string]string)
	vmCount := make(map[string]int)
	for _, snapshot := range snapshots {
		vmID := snapshot.BackupID
		vmNameMapping[vmID] = snapshot.Comment
		vmCount[vmID]++
	}

	for vmID, count := range vmCount {
		ch <- prometheus.MustNewConstMetric(
			snapshot_vm_count, prometheus.GaugeValue, float64(count), datastore, namespace, vmID, vmNameMapping[vmID],
		)

		lastTimeStamp, lastVerify, err := findLastSnapshotWithBackupID(snapshots, vmID)
		if err != nil {
			return err
		}
		lastVerifyBool := 0
		if lastVerify == "ok" {
			lastVerifyBool = 1
		}
		ch <- prometheus.MustNewConstMetric(
			snapshot_vm_last_timestamp, prometheus.GaugeValue, float64(lastTimeStamp), datastore, namespace, vmID, vmNameMapping[vmID],
		)
		ch <- prometheus.MustNewConstMetric(
			snapshot_vm_last_verify, prometheus.GaugeValue, float64(lastVerifyBool), datastore, namespace, vmID, vmNameMapping[vmID],
		)
	}

	return nil
}

func findLastSnapshotWithBackupID(snapshots []snapshotItem, backupID string) (int64, string, error) {
	// find biggest value of backupTime of backupID in response array
	var lastTimeStamp int64
	var lastVerify string
	for _, snapshot := range snapshots {
		if snapshot.BackupID == backupID {
			if snapshot.BackupTime > lastTimeStamp {
				lastTimeStamp = snapshot.BackupTime
				lastVerify = snapshot.Verification.State
			}
		}
	}

	// if lastTimeStamp is still 0, no snapshot was found
	if lastTimeStamp != 0 {
		return lastTimeStamp, lastVerify, nil
	}

	return 0, "", fmt.Errorf("no snapshot found with backup ID %s", backupID)
}
