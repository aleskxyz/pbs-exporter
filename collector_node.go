package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func hostDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(promNamespace, "", name), help, nil, nil)
}

var (
	subscription_status = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "host_subscription_status"),
		"The subscription status of the host.",
		[]string{"status"}, nil,
	)
	subscription_info = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "host_subscription_info"),
		"The subscription info of the host.",
		[]string{"productname", "status"}, nil,
	)
	subscription_due_timestamp_seconds = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "host_subscription_due_timestamp_seconds"),
		"The subscription next due timestamp (unix seconds) of the host.",
		[]string{"productname"}, nil,
	)
	host_cpu_usage      = hostDesc("host_cpu_usage", "The CPU usage of the host.")
	host_memory_free    = hostDesc("host_memory_free", "The free memory of the host.")
	host_memory_total   = hostDesc("host_memory_total", "The total memory of the host.")
	host_memory_used    = hostDesc("host_memory_used", "The used memory of the host.")
	host_swap_free      = hostDesc("host_swap_free", "The free swap of the host.")
	host_swap_total     = hostDesc("host_swap_total", "The total swap of the host.")
	host_swap_used      = hostDesc("host_swap_used", "The used swap of the host.")
	host_disk_available = hostDesc("host_disk_available", "The available disk of the local root disk in bytes.")
	host_disk_total     = hostDesc("host_disk_total", "The total disk of the local root disk in bytes.")
	host_disk_used      = hostDesc("host_disk_used", "The used disk of the local root disk in bytes.")
	host_uptime         = hostDesc("host_uptime", "The uptime of the host.")
	host_io_wait        = hostDesc("host_io_wait", "The io wait of the host.")
	host_load1          = hostDesc("host_load1", "The load for 1 minute of the host.")
	host_load5          = hostDesc("host_load5", "The load for 5 minutes of the host.")
	host_load15         = hostDesc("host_load15", "The load for 15 minutes of the host.")
)

// subscriptionStatuses are the values of pbs_host_subscription_status.
var subscriptionStatuses = []string{"new", "notfound", "active", "invalid", "expired", "suspended"}

type nodeStatus struct {
	CPU float64 `json:"cpu"`
	Mem struct {
		Free  int64 `json:"free"`
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"memory"`
	Swap struct {
		Free  int64 `json:"free"`
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"swap"`
	Disk struct {
		Avail int64 `json:"avail"`
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"root"`
	Load   []float64 `json:"loadavg"`
	Uptime int64     `json:"uptime"`
	Wait   float64   `json:"wait"`
}

// NOTE: According to the api documentation, we have to provide the node name (won't work with the node ip),
// but it seems to work with any name, so we just use "localhost" here.
// see: https://pbs.proxmox.com/docs/api-viewer/index.html#/nodes/{node}
var nodeCollector = &collector{
	name:      "node",
	enabled:   true,
	privilege: "Sys.Audit on /system",
	descs: []*prometheus.Desc{
		subscription_status, subscription_info, subscription_due_timestamp_seconds,
		host_cpu_usage, host_memory_free, host_memory_total, host_memory_used,
		host_swap_free, host_swap_total, host_swap_used,
		host_disk_available, host_disk_total, host_disk_used,
		host_uptime, host_io_wait, host_load1, host_load5, host_load15,
	},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return errors.Join(collectNodeStatus(ctx, s, ch), collectSubscription(ctx, s, ch))
	},
}

func collectNodeStatus(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var st nodeStatus
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "status"), nil, &st); err != nil {
		return err
	}
	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
	}
	gauge(host_cpu_usage, st.CPU)
	gauge(host_memory_free, float64(st.Mem.Free))
	gauge(host_memory_total, float64(st.Mem.Total))
	gauge(host_memory_used, float64(st.Mem.Used))
	gauge(host_swap_free, float64(st.Swap.Free))
	gauge(host_swap_total, float64(st.Swap.Total))
	gauge(host_swap_used, float64(st.Swap.Used))
	gauge(host_disk_available, float64(st.Disk.Avail))
	gauge(host_disk_total, float64(st.Disk.Total))
	gauge(host_disk_used, float64(st.Disk.Used))
	gauge(host_uptime, float64(st.Uptime))
	gauge(host_io_wait, st.Wait)
	// a missing or short loadavg would panic otherwise
	if len(st.Load) >= 3 {
		gauge(host_load1, st.Load[0])
		gauge(host_load5, st.Load[1])
		gauge(host_load15, st.Load[2])
	}
	return nil
}

func collectSubscription(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var sub map[string]any
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "subscription"), nil, &sub); err != nil {
		return err
	}

	statusStr := ""
	productName := "unknown"
	if v, ok := sub["status"]; ok && v != nil {
		statusStr = fmt.Sprintf("%v", v)
	}
	if v, ok := sub["productname"]; ok && v != nil {
		productName = fmt.Sprintf("%v", v)
	}

	ch <- prometheus.MustNewConstMetric(subscription_info, prometheus.GaugeValue, 1, productName, statusStr)

	// no series without a due date: 0 would read as 1970 and fire every
	// "subscription expires soon" alert
	if v, ok := sub["nextduedate"].(string); ok {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			ch <- prometheus.MustNewConstMetric(subscription_due_timestamp_seconds, prometheus.GaugeValue, float64(t.Unix()), productName)
		}
	}

	for _, st := range subscriptionStatuses {
		val := 0.0
		if statusStr == st {
			val = 1
		}
		ch <- prometheus.MustNewConstMetric(subscription_status, prometheus.GaugeValue, val, st)
	}
	return nil
}
