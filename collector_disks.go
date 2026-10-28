package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	diskSmartStatus = newDesc("disk", "smart_status",
		"SMART health of a disk as PBS reports it (status passed, failed or unknown), always 1.",
		"disk", "model", "serial", "status")
	diskWearout = newDesc("disk", "wearout_percent",
		"The wearout field PBS reports for the disk: the remaining rated life of an SSD in percent (100 = new); omitted when PBS reports none.",
		"disk")
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
			gauge(ch, diskSmartStatus, 1, d.Name, d.Model, d.Serial, d.Status)
			optGauge(ch, diskWearout, d.Wearout, d.Name)
		}
		return nil
	},
}
