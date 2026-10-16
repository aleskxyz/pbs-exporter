package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	zfsPoolHealthy = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "zfs_pool", "healthy"),
		"Whether the ZFS pool is ONLINE; health is the state PBS reports. PBS does not list pools it can not read the usage of (e.g. FAULTED or UNAVAIL), so watch for pools that disappear.",
		[]string{"pool", "health"}, nil,
	)
	zfsPoolSize = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "zfs_pool", "size_bytes"),
		"Size of the ZFS pool.",
		[]string{"pool"}, nil,
	)
	zfsPoolAllocated = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "zfs_pool", "allocated_bytes"),
		"Allocated space of the ZFS pool.",
		[]string{"pool"}, nil,
	)
	zfsPoolFragmentation = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "zfs_pool", "fragmentation_ratio"),
		"Fragmentation of the free space of the ZFS pool (0-1).",
		[]string{"pool"}, nil,
	)
)

// zfsPool is an entry of /nodes/localhost/disks/zfs.
type zfsPool struct {
	Name   string   `json:"name"`
	Health string   `json:"health"`
	Size   int64    `json:"size"`
	Alloc  int64    `json:"alloc"`
	Frag   *float64 `json:"frag"`
}

var zfsCollector = &collector{
	name:      "zfs",
	enabled:   true,
	privilege: "Sys.Audit on /system/disks",
	descs:     []*prometheus.Desc{zfsPoolHealthy, zfsPoolSize, zfsPoolAllocated, zfsPoolFragmentation},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var pools []zfsPool
		if err := s.api.get(ctx, apiPath("nodes", "localhost", "disks", "zfs"), nil, &pools); err != nil {
			return err
		}
		for _, p := range pools {
			healthy := 0.0
			if p.Health == "ONLINE" {
				healthy = 1
			}
			ch <- prometheus.MustNewConstMetric(zfsPoolHealthy, prometheus.GaugeValue, healthy, p.Name, p.Health)
			ch <- prometheus.MustNewConstMetric(zfsPoolSize, prometheus.GaugeValue, float64(p.Size), p.Name)
			ch <- prometheus.MustNewConstMetric(zfsPoolAllocated, prometheus.GaugeValue, float64(p.Alloc), p.Name)
			if p.Frag != nil {
				ch <- prometheus.MustNewConstMetric(zfsPoolFragmentation, prometheus.GaugeValue, *p.Frag/100, p.Name)
			}
		}
		return nil
	},
}
