package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var nodeTimeOffset = prometheus.NewDesc(
	prometheus.BuildFQName(promNamespace, "node", "time_offset_seconds"),
	"Clock of the PBS node minus the clock of the exporter, accurate to about a second plus half the round trip.",
	nil, nil,
)

type nodeTime struct {
	// Time is UTC; localtime is shifted by the time zone and is not used.
	Time int64 `json:"time"`
}

var timeCollector = &collector{
	name:    "time",
	enabled: true,
	// /nodes/{node}/time is readable by anybody
	descs: []*prometheus.Desc{nodeTimeOffset},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var t nodeTime
		sent, received, err := s.api.getTimed(ctx, apiPath("nodes", "localhost", "time"), nil, &t)
		if err != nil {
			return err
		}
		// compare with the middle of the round trip, so it does not count as
		// offset; PBS reports whole seconds, rounded down, so the node's clock
		// is on average half a second later than t.Time
		mid := sent.Add(received.Sub(sent) / 2)
		offset := time.Unix(t.Time, 0).Add(500 * time.Millisecond).Sub(mid)
		ch <- prometheus.MustNewConstMetric(nodeTimeOffset, prometheus.GaugeValue, offset.Seconds())
		return nil
	},
}
