package main

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

func datastoreDesc(subsystem, name, help string, extra ...string) *prometheus.Desc {
	return newDesc(subsystem, name, help, append([]string{"datastore"}, extra...)...)
}

var (
	datastoreAvailable = datastoreDesc("datastore", "available_bytes",
		"The available bytes of the underlying storage of the datastore, as PBS reports them (may be negative when the storage is overcommitted).")
	datastoreSize = datastoreDesc("datastore", "size_bytes",
		"The size of the underlying storage of the datastore in bytes.")
	datastoreUsed = datastoreDesc("datastore", "used_bytes",
		"The used bytes of the underlying storage of the datastore.")
	datastoreEstimatedFull = datastoreDesc("datastore", "estimated_full_timestamp_seconds",
		"When PBS expects the datastore to be full (estimated-full-date as PBS reports it; a date in the past means PBS has no estimate).")
	datastoreMounted = datastoreDesc("datastore", "mounted",
		"Whether a removable datastore is mounted; omitted for other datastores.")
	datastoreError = datastoreDesc("datastore", "error",
		"Whether PBS reports an error for the datastore in /status/datastore-usage (e.g. it can not open it: failed storage, offline maintenance).")
	datastoreMaintenance = datastoreDesc("datastore", "maintenance",
		"Present while the datastore is in maintenance mode, always 1.", "mode")
	datastoreInfo = datastoreDesc("datastore", "info",
		"Settings of the datastore, always 1.", "verify_new", "notification_mode", "gc_schedule", "backend_type")
	datastoreKeep = datastoreDesc("datastore", "keep",
		"Retention setting of the datastore.", "keep")
	gcDiskBytes = datastoreDesc("gc", "disk_bytes",
		"Bytes used by chunks, as of the last garbage collection (0 before the first one).")
	gcIndexDataBytes = datastoreDesc("gc", "index_data_bytes",
		"Bytes referenced by indexes, as of the last garbage collection (divided by the disk bytes: deduplication factor).")
	gcPendingBytes = datastoreDesc("gc", "pending_bytes",
		"Bytes waiting for removal by the next garbage collection.")
	gcRemovedBytes = datastoreDesc("gc", "removed_bytes",
		"Bytes removed by the last garbage collection.")
	gcBadChunks = datastoreDesc("gc", "bad_chunks",
		"Corrupt chunks found by the last garbage collection: still_bad are still referenced by snapshots, removed_bad were removed.", "state")
)

// datastoreUsage is an entry of /status/datastore-usage.
type datastoreUsage struct {
	Store string `json:"store"`
	// Avail, Total and Used are not set when PBS can not tell: an unmounted
	// removable datastore, one that can not be opened (Error is set, e.g.
	// offline maintenance) or one the token only sees through a namespace.
	Avail       *int64 `json:"avail"`
	Total       *int64 `json:"total"`
	Used        *int64 `json:"used"`
	Error       string `json:"error"`
	MountStatus string `json:"mount-status"`
	// EstimatedFullDate is unix seconds; PBS reports a date in the past or
	// none when it has no estimate.
	EstimatedFullDate *int64    `json:"estimated-full-date"`
	GCStatus          *gcStatus `json:"gc-status"`
}

// gcStatus are the statistics of the last garbage collection.
type gcStatus struct {
	DiskBytes      *int64 `json:"disk-bytes"`
	IndexDataBytes *int64 `json:"index-data-bytes"`
	PendingBytes   *int64 `json:"pending-bytes"`
	RemovedBytes   *int64 `json:"removed-bytes"`
	StillBad       *int64 `json:"still-bad"`
	RemovedBad     *int64 `json:"removed-bad"`
}

// datastoreConfig is an entry of /config/datastore.
type datastoreConfig struct {
	Name             string `json:"name"`
	VerifyNew        *bool  `json:"verify-new"`
	NotificationMode string `json:"notification-mode"`
	GCSchedule       string `json:"gc-schedule"`
	keepOptions
}

// datastoreListItem is an entry of /admin/datastore.
type datastoreListItem struct {
	Store string `json:"store"`
	// Maintenance is the maintenance mode as property string, e.g.
	// "type=offline,message=disk swap"; empty without maintenance.
	Maintenance string `json:"maintenance"`
	BackendType string `json:"backend-type"`
}

func (s *scrapeState) getDatastoreUsage(ctx context.Context) ([]datastoreUsage, error) {
	return s.datastoreUsage.get(func() ([]datastoreUsage, error) {
		var out []datastoreUsage
		err := s.api.get(ctx, apiPath("status", "datastore-usage"), nil, &out)
		return out, err
	})
}

func (s *scrapeState) getDatastoreList(ctx context.Context) ([]datastoreListItem, error) {
	return s.datastoreList.get(func() ([]datastoreListItem, error) {
		var out []datastoreListItem
		err := s.api.get(ctx, apiPath("admin", "datastore"), nil, &out)
		return out, err
	})
}

// maintenanceModes returns the maintenance mode type ("offline",
// "read-only", ...) of the datastores of /admin/datastore in maintenance.
func maintenanceModes(list []datastoreListItem) map[string]string {
	modes := map[string]string{}
	for _, d := range list {
		if d.Maintenance != "" {
			modes[d.Store] = maintenanceType(d.Maintenance)
		}
	}
	return modes
}

var datastoresCollector = &collector{
	name:      "datastores",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs: []*prometheus.Desc{
		datastoreAvailable, datastoreSize, datastoreUsed, datastoreEstimatedFull, datastoreError, datastoreMounted, datastoreMaintenance,
		datastoreInfo, datastoreKeep, gcDiskBytes, gcIndexDataBytes, gcPendingBytes, gcRemovedBytes, gcBadChunks,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		// /admin/datastore gives the maintenance mode and backend of each
		// datastore, /status/datastore-usage the usage
		list, listErr := s.getDatastoreList(ctx)
		modes := maintenanceModes(list)
		for store, mode := range modes {
			gauge(ch, datastoreMaintenance, 1, store, mode)
		}
		usage, usageErr := s.getDatastoreUsage(ctx)
		for _, d := range usage {
			// a datastore in maintenance is expected to fail
			_, inMaintenance := modes[d.Store]
			collectDatastoreUsage(d, inMaintenance, ch)
		}
		// PBS leaves out the datastores the token can not audit
		return joinUnique(listErr, usageErr, collectDatastoreConfig(ctx, s, list, listErr == nil, ch),
			s.privilegeGap(ctx, permissions.canAuditDatastores, errDatastoresFiltered))
	},
}

func collectDatastoreUsage(d datastoreUsage, inMaintenance bool, ch chan<- prometheus.Metric) {
	if d.Error != "" {
		level := slog.LevelWarn
		if inMaintenance {
			level = slog.LevelDebug
		}
		slog.Log(context.Background(), level, "PBS reports an error for the datastore",
			"datastore", sanitize(d.Store), "err", sanitize(d.Error))
		gauge(ch, datastoreError, 1, d.Store)
	} else {
		gauge(ch, datastoreError, 0, d.Store)
	}
	// values PBS does not report have no series: 0 bytes would read as a
	// full datastore
	optGauge(ch, datastoreAvailable, d.Avail, d.Store)
	optGauge(ch, datastoreSize, d.Total, d.Store)
	optGauge(ch, datastoreUsed, d.Used, d.Store)
	optGauge(ch, datastoreEstimatedFull, d.EstimatedFullDate, d.Store)
	switch d.MountStatus {
	case "mounted":
		gauge(ch, datastoreMounted, 1, d.Store)
	case "notmounted":
		gauge(ch, datastoreMounted, 0, d.Store)
	}
	if gc := d.GCStatus; gc != nil {
		optGauge(ch, gcDiskBytes, gc.DiskBytes, d.Store)
		optGauge(ch, gcIndexDataBytes, gc.IndexDataBytes, d.Store)
		optGauge(ch, gcPendingBytes, gc.PendingBytes, d.Store)
		optGauge(ch, gcRemovedBytes, gc.RemovedBytes, d.Store)
		optGauge(ch, gcBadChunks, gc.StillBad, d.Store, "still_bad")
		optGauge(ch, gcBadChunks, gc.RemovedBad, d.Store, "removed_bad")
	}
}

// collectDatastoreConfig exports the settings of /config/datastore.
// pbs_datastore_info has the backend of /admin/datastore, so it is left out
// when that list could not be read (listed is false).
func collectDatastoreConfig(ctx context.Context, s *scrapeState, list []datastoreListItem, listed bool, ch chan<- prometheus.Metric) error {
	var configs []datastoreConfig
	if err := s.api.get(ctx, apiPath("config", "datastore"), nil, &configs); err != nil {
		return err
	}
	backendTypes := map[string]string{}
	for _, d := range list {
		backendTypes[d.Store] = d.BackendType
	}
	for _, c := range configs {
		if listed {
			gauge(ch, datastoreInfo, 1,
				c.Name, optBool(c.VerifyNew), c.NotificationMode, c.GCSchedule, backendTypes[c.Name])
		}
		for keep, v := range c.set() {
			gauge(ch, datastoreKeep, float64(v), c.Name, keep)
		}
	}
	return nil
}
