package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var nodeTime = newDesc("node", "time_seconds",
	"The clock of the PBS node (/nodes/localhost/time); compare with timestamp(pbs_node_time_seconds), the scrape time, for the clock offset.")

type nodeTimeResponse struct {
	// Time is UTC; localtime is shifted by the time zone and is not used.
	Time *int64 `json:"time"`
}

var timeCollector = &collector{
	name:    "time",
	enabled: true,
	// /nodes/{node}/time is readable by anybody
	descs: []*prometheus.Desc{nodeTime},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var t nodeTimeResponse
		if err := s.api.get(ctx, apiPath("nodes", "localhost", "time"), nil, &t); err != nil {
			return err
		}
		optGauge(ch, nodeTime, t.Time)
		return nil
	},
}
