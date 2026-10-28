package main

import (
	"context"
	"encoding/json"

	"github.com/prometheus/client_golang/prometheus"
)

var aptUpdatesAvailable = newDesc("apt", "updates_available",
	"Number of packages with a pending update, as of the last package list refresh on the node.")

var aptCollector = &collector{
	name:      "apt",
	enabled:   true,
	privilege: "Sys.Audit on /",
	descs:     []*prometheus.Desc{aptUpdatesAvailable},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		// only the number of entries is exported
		var updates []json.RawMessage
		if err := s.api.get(ctx, apiPath("nodes", "localhost", "apt", "update"), nil, &updates); err != nil {
			return err
		}
		gauge(ch, aptUpdatesAvailable, float64(len(updates)))
		return nil
	},
}
