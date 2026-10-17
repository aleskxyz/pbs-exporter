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
	descs:     []*prometheus.Desc{available, size, used},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		usage, err := s.getDatastoreUsage(ctx)
		if err != nil {
			return err
		}
		for _, d := range usage {
			if d.Error != "" {
				slog.Debug("datastore without usage", "datastore", sanitize(d.Store), "err", sanitize(d.Error))
			}
			// no series without a value: 0 bytes would read as a full datastore
			for desc, v := range map[*prometheus.Desc]*int64{available: d.Avail, size: d.Total, used: d.Used} {
				if v != nil {
					ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(*v), d.Store)
				}
			}
		}
		return nil
	},
}
