package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var version = prometheus.NewDesc(
	prometheus.BuildFQName(promNamespace, "", "version"),
	"Version of the PBS installation.",
	[]string{"version", "repoid", "release"}, nil,
)

type versionResponse struct {
	Release string `json:"release"`
	Repoid  string `json:"repoid"`
	Version string `json:"version"`
}

var versionCollector = &collector{
	name:    versionCollectorName,
	enabled: true,
	descs:   []*prometheus.Desc{version},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var v versionResponse
		if err := s.api.get(ctx, apiPath("version"), nil, &v); err != nil {
			return err
		}
		ch <- prometheus.MustNewConstMetric(version, prometheus.GaugeValue, 1, v.Version, v.Repoid, v.Release)
		return nil
	},
}
