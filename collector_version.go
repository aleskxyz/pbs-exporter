package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var versionInfo = newDesc("", "version_info",
	"Version of the PBS installation.",
	"version", "repoid", "release")

type versionResponse struct {
	Release string `json:"release"`
	Repoid  string `json:"repoid"`
	Version string `json:"version"`
}

// versionCollector can not be disabled, pbs_up is derived from it.
var versionCollector = &collector{
	name:     "version",
	enabled:  true,
	required: true,
	descs:    []*prometheus.Desc{versionInfo},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var v versionResponse
		if err := s.api.get(ctx, apiPath("version"), nil, &v); err != nil {
			return err
		}
		gauge(ch, versionInfo, 1, v.Version, v.Repoid, v.Release)
		return nil
	},
}
