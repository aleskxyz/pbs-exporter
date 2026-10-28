package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	zfsPoolHealth = newDesc("zfs_pool", "health_info",
		"Health of the ZFS pool as PBS reports it (ONLINE, DEGRADED, ...), always 1. PBS does not list pools it can not read the usage of (e.g. FAULTED or UNAVAIL), so watch for pools that disappear.",
		"pool", "health")
	zfsPoolSize = newDesc("zfs_pool", "size_bytes",
		"Size of the ZFS pool.",
		"pool")
	zfsPoolAllocated = newDesc("zfs_pool", "allocated_bytes",
		"Allocated space of the ZFS pool.",
		"pool")
	zfsPoolFragmentation = newDesc("zfs_pool", "fragmentation_ratio",
		"Fragmentation of the free space of the ZFS pool (0-1).",
		"pool")
)

// zfsPool is an entry of /nodes/localhost/disks/zfs.
type zfsPool struct {
	Name   string   `json:"name"`
	Health string   `json:"health"`
	Size   *int64   `json:"size"`
	Alloc  *int64   `json:"alloc"`
	Frag   *float64 `json:"frag"`
}

var zfsCollector = &collector{
	name:      "zfs",
	enabled:   true,
	privilege: "Sys.Audit on /system/disks",
	descs:     []*prometheus.Desc{zfsPoolHealth, zfsPoolSize, zfsPoolAllocated, zfsPoolFragmentation},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var pools []zfsPool
		if err := s.api.get(ctx, apiPath("nodes", "localhost", "disks", "zfs"), nil, &pools); err != nil {
			return err
		}
		for _, p := range pools {
			gauge(ch, zfsPoolHealth, 1, p.Name, p.Health)
			optGauge(ch, zfsPoolSize, p.Size, p.Name)
			optGauge(ch, zfsPoolAllocated, p.Alloc, p.Name)
			// PBS reports percent
			if p.Frag != nil {
				gauge(ch, zfsPoolFragmentation, *p.Frag/100, p.Name)
			}
		}
		return nil
	},
}
