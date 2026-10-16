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
		prometheus.BuildFQName(promNamespace, "", "available"),
		"The available bytes of the underlying storage.",
		[]string{"datastore"}, nil,
	)
	size = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "size"),
		"The size of the underlying storage in bytes.",
		[]string{"datastore"}, nil,
	)
	used = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "used"),
		"The used bytes of the underlying storage.",
		[]string{"datastore"}, nil,
	)
)

// datastoreUsage is an entry of /status/datastore-usage.
type datastoreUsage struct {
	Store       string `json:"store"`
	Avail       int64  `json:"avail"`
	Total       int64  `json:"total"`
	Used        int64  `json:"used"`
	MountStatus string `json:"mount-status"`
	BackendType string `json:"backend-type"`
}

// datastoreListItem is an entry of /admin/datastore.
type datastoreListItem struct {
	Store string `json:"store"`
	// Maintenance is the maintenance mode as property string, e.g.
	// "type=offline,message=disk swap"; empty without maintenance.
	Maintenance string `json:"maintenance"`
	MountStatus string `json:"mount-status"`
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
// ("offline", "read-only", ...); the key may be omitted ("offline,message=x").
func maintenanceType(mode string) string {
	for _, part := range strings.Split(mode, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return strings.TrimSpace(k)
		}
		if strings.TrimSpace(k) == "type" {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
	return strings.Contains(msg, "being deleted") ||
		strings.Contains(msg, "maintenance mode") ||
		strings.Contains(msg, "not mounted")
}

var datastoresCollector = &collector{
	name:      "datastores",
	enabled:   true,
	privilege: "Datastore.Audit on /datastore",
	descs:     []*prometheus.Desc{available, size, used},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		usage, err := s.getDatastoreUsage(ctx)
		if err != nil {
			return err
		}
		for _, d := range usage {
			ch <- prometheus.MustNewConstMetric(available, prometheus.GaugeValue, float64(d.Avail), d.Store)
			ch <- prometheus.MustNewConstMetric(size, prometheus.GaugeValue, float64(d.Total), d.Store)
			ch <- prometheus.MustNewConstMetric(used, prometheus.GaugeValue, float64(d.Used), d.Store)
		}
		return nil
	},
}
