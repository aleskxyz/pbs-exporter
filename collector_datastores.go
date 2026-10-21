package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	available = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "available_bytes"),
		"The available bytes of the underlying storage of the datastore.",
		[]string{"datastore"}, nil,
	)
	size = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "size_bytes"),
		"The size of the underlying storage of the datastore in bytes.",
		[]string{"datastore"}, nil,
	)
	used = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "used_bytes"),
		"The used bytes of the underlying storage of the datastore.",
		[]string{"datastore"}, nil,
	)
	datastoreEstimatedFull = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "estimated_full_timestamp_seconds"),
		"When PBS expects the datastore to be full; omitted without an estimate or when it is in the past.",
		[]string{"datastore"}, nil,
	)
	datastoreMounted = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "mounted"),
		"Whether a removable datastore is mounted; omitted for other datastores.",
		[]string{"datastore"}, nil,
	)
	datastoreAccessible = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "accessible"),
		"Whether PBS can open the datastore; 0 when it reports an error instead of the usage (e.g. failed storage, offline maintenance). Omitted for unmounted removable datastores.",
		[]string{"datastore"}, nil,
	)
	datastoreMaintenance = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "maintenance"),
		"Present while the datastore is in maintenance mode, always 1.",
		[]string{"datastore", "mode"}, nil,
	)
	datastoreInfo = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "info"),
		"Settings of the datastore, always 1.",
		[]string{"datastore", "verify_new", "notification_mode", "gc_schedule", "backend_type"}, nil,
	)
	datastoreKeep = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "datastore", "keep"),
		"Retention setting of the datastore.",
		[]string{"datastore", "keep"}, nil,
	)
	gcDiskBytes = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "gc", "disk_bytes"),
		"Bytes used by chunks, as of the last garbage collection.",
		[]string{"datastore"}, nil,
	)
	gcIndexDataBytes = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "gc", "index_data_bytes"),
		"Bytes referenced by indexes, as of the last garbage collection (divided by the disk bytes: deduplication factor).",
		[]string{"datastore"}, nil,
	)
	gcPendingBytes = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "gc", "pending_bytes"),
		"Bytes waiting for removal by the next garbage collection.",
		[]string{"datastore"}, nil,
	)
	gcRemovedBytes = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "gc", "removed_bytes"),
		"Bytes removed by the last garbage collection.",
		[]string{"datastore"}, nil,
	)
	gcBadChunks = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "gc", "bad_chunks"),
		"Corrupt chunks found by the last garbage collection: still_bad are still referenced by snapshots, removed_bad were removed.",
		[]string{"datastore", "state"}, nil,
	)
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
	BackendType string `json:"backend-type"`
	// EstimatedFullDate is unix seconds, PBS reports a date in the past or
	// none when it can not tell.
	EstimatedFullDate *int64    `json:"estimated-full-date"`
	GCStatus          *gcStatus `json:"gc-status"`
}

// gcStatus are the statistics of the last garbage collection.
type gcStatus struct {
	// UPID is the task of the last garbage collection, empty if there was none.
	UPID           string `json:"upid"`
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
}

func (s *scrapeState) getDatastoreUsage(ctx context.Context) ([]datastoreUsage, error) {
	return s.datastoreUsage.get(struct{}{}, func() ([]datastoreUsage, error) {
		var out []datastoreUsage
		err := s.api.get(ctx, apiPath("status", "datastore-usage"), nil, &out)
		return out, err
	})
}

func (s *scrapeState) getDatastoreList(ctx context.Context) ([]datastoreListItem, error) {
	return s.datastoreList.get(struct{}{}, func() ([]datastoreListItem, error) {
		var out []datastoreListItem
		err := s.api.get(ctx, apiPath("admin", "datastore"), nil, &out)
		return out, err
	})
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

// readableDatastores returns the datastores whose content can be read: not
// unmounted and not in a maintenance mode that blocks reading. The usage
// endpoint lists the datastores, /admin/datastore adds the maintenance mode;
// if that call fails, its error is returned with all datastores that are
// mounted.
func (s *scrapeState) readableDatastores(ctx context.Context) ([]string, error) {
	usage, err := s.getDatastoreUsage(ctx)
	if err != nil {
		return nil, err
	}
	list, listErr := s.getDatastoreList(ctx)
	maintenance := map[string]string{}
	for _, d := range list {
		if d.Maintenance != "" {
			maintenance[d.Store] = maintenanceType(d.Maintenance)
		}
	}
	var stores []string
	for _, d := range usage {
		switch mode, inMaintenance := maintenance[d.Store]; {
		case d.MountStatus == "notmounted":
			slog.Debug("skipping unmounted datastore", "datastore", sanitize(d.Store))
		case inMaintenance && mode != "read-only":
			slog.Info("skipping datastore in maintenance mode", "datastore", sanitize(d.Store), "mode", sanitize(mode))
		default:
			stores = append(stores, d.Store)
		}
	}
	return stores, listErr
}

// isDatastoreUnavailable reports whether err says that a datastore can not be
// read right now (deleted, in maintenance, unmounted), for PBS versions whose
// lists do not tell this up front.
func isDatastoreUnavailable(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	// "datastore is being deleted", "... being unmounted", "... in offline
	// maintenance mode", "... is not mounted"
	return strings.Contains(msg, "datastore is being") ||
		strings.Contains(msg, "maintenance mode") ||
		strings.Contains(msg, "not mounted")
}

var datastoresCollector = &collector{
	name:      "datastores",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs: []*prometheus.Desc{
		available, size, used, datastoreEstimatedFull, datastoreAccessible, datastoreMounted, datastoreMaintenance,
		datastoreInfo, datastoreKeep, gcDiskBytes, gcIndexDataBytes, gcPendingBytes, gcRemovedBytes, gcBadChunks,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		usage, err := s.getDatastoreUsage(ctx)
		if err != nil {
			return err
		}
		// a datastore in maintenance is expected to fail; the error of the
		// list is reported by collectDatastoreMaintenance
		list, _ := s.getDatastoreList(ctx)
		inMaintenance := map[string]bool{}
		for _, d := range list {
			inMaintenance[d.Store] = d.Maintenance != ""
		}
		backendTypes := map[string]string{}
		for _, d := range usage {
			backendTypes[d.Store] = d.BackendType
			collectDatastoreUsage(s, d, inMaintenance[d.Store], ch)
		}
		return errors.Join(
			collectDatastoreMaintenance(ctx, s, ch),
			collectDatastoreConfig(ctx, s, backendTypes, ch),
		)
	},
}

func collectDatastoreUsage(s *scrapeState, d datastoreUsage, inMaintenance bool, ch chan<- prometheus.Metric) {
	gauge := func(desc *prometheus.Desc, v float64, extra ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, append([]string{d.Store}, extra...)...)
	}
	optGauge := func(desc *prometheus.Desc, v *int64, extra ...string) {
		if v != nil {
			gauge(desc, float64(*v), extra...)
		}
	}
	// an unmounted removable datastore can not be opened either, that is
	// what pbs_datastore_mounted tells
	if d.MountStatus != "notmounted" {
		accessible := 1.0
		if d.Error != "" {
			accessible = 0
			level := slog.LevelWarn
			if inMaintenance {
				level = slog.LevelDebug
			}
			slog.Log(context.Background(), level, "PBS can not open the datastore",
				"datastore", sanitize(d.Store), "err", sanitize(d.Error))
		}
		gauge(datastoreAccessible, accessible)
	}
	// no series without a value: 0 bytes would read as a full datastore
	optGauge(available, d.Avail)
	optGauge(size, d.Total)
	optGauge(used, d.Used)
	// a date in the past means PBS has no useful estimate
	if d.EstimatedFullDate != nil && *d.EstimatedFullDate > s.now.Unix() {
		gauge(datastoreEstimatedFull, float64(*d.EstimatedFullDate))
	}
	switch d.MountStatus {
	case "mounted":
		gauge(datastoreMounted, 1)
	case "notmounted":
		gauge(datastoreMounted, 0)
	}
	// without a garbage collection so far, PBS reports zeros
	if gc := d.GCStatus; gc != nil && gc.UPID != "" {
		optGauge(gcDiskBytes, gc.DiskBytes)
		optGauge(gcIndexDataBytes, gc.IndexDataBytes)
		optGauge(gcPendingBytes, gc.PendingBytes)
		optGauge(gcRemovedBytes, gc.RemovedBytes)
		optGauge(gcBadChunks, gc.StillBad, "still_bad")
		optGauge(gcBadChunks, gc.RemovedBad, "removed_bad")
	}
}

func collectDatastoreMaintenance(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	list, err := s.getDatastoreList(ctx)
	if err != nil {
		return err
	}
	for _, d := range list {
		if d.Maintenance != "" {
			ch <- prometheus.MustNewConstMetric(datastoreMaintenance, prometheus.GaugeValue, 1, d.Store, maintenanceType(d.Maintenance))
		}
	}
	return nil
}

func collectDatastoreConfig(ctx context.Context, s *scrapeState, backendTypes map[string]string, ch chan<- prometheus.Metric) error {
	var configs []datastoreConfig
	if err := s.api.get(ctx, apiPath("config", "datastore"), nil, &configs); err != nil {
		return err
	}
	for _, c := range configs {
		ch <- prometheus.MustNewConstMetric(datastoreInfo, prometheus.GaugeValue, 1,
			c.Name, optBool(c.VerifyNew), c.NotificationMode, c.GCSchedule, backendTypes[c.Name])
		for keep, v := range c.set() {
			ch <- prometheus.MustNewConstMetric(datastoreKeep, prometheus.GaugeValue, float64(v), c.Name, keep)
		}
	}
	return nil
}
