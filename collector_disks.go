package main

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	diskSmartStatus = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "disk", "smart_status"),
		"SMART health of a disk as PBS reports it (status passed, failed or unknown), always 1.",
		[]string{"disk", "model", "serial", "status"}, nil,
	)
	diskWearout = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "disk", "wearout_ratio"),
		"Used up share of the SSD's rated life (0 = new, 1 = worn out), as the PBS web interface shows it; omitted when PBS reports none.",
		[]string{"disk"}, nil,
	)
)

// diskItem is an entry of /nodes/localhost/disks/list.
type diskItem struct {
	Name   string `json:"name"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
	Status string `json:"status"`
	// Wearout is the remaining life in percent (100 = new).
	Wearout *float64 `json:"wearout"`
}

var disksCollector = &collector{
	name:      "disks",
	enabled:   false,
	privilege: "Sys.Audit on /system/disks",
	descs:     []*prometheus.Desc{diskSmartStatus, diskWearout},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		var disks []diskItem
		if err := s.api.get(ctx, apiPath("nodes", "localhost", "disks", "list"), nil, &disks); err != nil {
			return err
		}
		for _, d := range disks {
			status := strings.ToLower(d.Status)
			if status == "" {
				status = "unknown"
			}
			ch <- prometheus.MustNewConstMetric(diskSmartStatus, prometheus.GaugeValue, 1, d.Name, d.Model, d.Serial, status)
			if d.Wearout != nil {
				ch <- prometheus.MustNewConstMetric(diskWearout, prometheus.GaugeValue, (100-*d.Wearout)/100, d.Name)
			}
		}
		return nil
	},
}
