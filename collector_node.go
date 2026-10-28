package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	nodeSubscriptionInfo = newDesc("node", "subscription_info",
		"The subscription info of the host.",
		"productname", "status")
	nodeInfo = newDesc("node", "info",
		"Identity of the PBS: the SHA-256 fingerprint of its TLS certificate (lower case, colon separated), always 1.",
		"fingerprint")
	nodeKernel = newDesc("node", "kernel_info",
		"The running kernel of the PBS node (current-kernel of /nodes/localhost/status), always 1.",
		"release")
	nodeSubscriptionDue = newDesc("node", "subscription_due_timestamp_seconds",
		"The next due date of the subscription of the node.")
	nodeCPUUsage    = newDesc("node", "cpu_usage_ratio", "The CPU usage of the node (0-1).")
	nodeMemoryFree  = newDesc("node", "memory_free_bytes", "The free memory of the node.")
	nodeMemorySize  = newDesc("node", "memory_size_bytes", "The memory of the node.")
	nodeMemoryUsed  = newDesc("node", "memory_used_bytes", "The used memory of the node.")
	nodeSwapFree    = newDesc("node", "swap_free_bytes", "The free swap of the node.")
	nodeSwapSize    = newDesc("node", "swap_size_bytes", "The swap of the node.")
	nodeSwapUsed    = newDesc("node", "swap_used_bytes", "The used swap of the node.")
	nodeRootfsAvail = newDesc("node", "rootfs_available_bytes", "The available space of the root file system of the node.")
	nodeRootfsSize  = newDesc("node", "rootfs_size_bytes", "The size of the root file system of the node.")
	nodeRootfsUsed  = newDesc("node", "rootfs_used_bytes", "The used space of the root file system of the node.")
	nodeUptime      = newDesc("node", "uptime_seconds", "The uptime of the node.")
	nodeIOWait      = newDesc("node", "io_wait_ratio", "The I/O wait of the node (0-1).")
	nodeLoad1       = newDesc("node", "load1", "The load average of the node over 1 minute.")
	nodeLoad5       = newDesc("node", "load5", "The load average of the node over 5 minutes.")
	nodeLoad15      = newDesc("node", "load15", "The load average of the node over 15 minutes.")
)

// nodeStatus is /nodes/localhost/status; values it does not report are nil.
type nodeStatus struct {
	CPU  *float64     `json:"cpu"`
	Mem  nodeCapacity `json:"memory"`
	Swap nodeCapacity `json:"swap"`
	Disk struct {
		Avail *int64 `json:"avail"`
		Total *int64 `json:"total"`
		Used  *int64 `json:"used"`
	} `json:"root"`
	Load          []float64 `json:"loadavg"`
	Uptime        *int64    `json:"uptime"`
	Wait          *float64  `json:"wait"`
	CurrentKernel struct {
		Release string `json:"release"`
	} `json:"current-kernel"`
	Info struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"info"`
}

// nodeCapacity is the memory or swap of /nodes/localhost/status.
type nodeCapacity struct {
	Free  *int64 `json:"free"`
	Total *int64 `json:"total"`
	Used  *int64 `json:"used"`
}

// NOTE: According to the api documentation, we have to provide the node name (won't work with the node ip),
// but it seems to work with any name, so we just use "localhost" here.
// see: https://pbs.proxmox.com/docs/api-viewer/index.html#/nodes/{node}
var nodeCollector = &collector{
	name:      "node",
	enabled:   true,
	privilege: "Sys.Audit on /system/status, Sys.Audit on / (subscription)",
	descs: []*prometheus.Desc{
		nodeInfo, nodeKernel, nodeSubscriptionInfo, nodeSubscriptionDue,
		nodeCPUUsage, nodeMemoryFree, nodeMemorySize, nodeMemoryUsed,
		nodeSwapFree, nodeSwapSize, nodeSwapUsed,
		nodeRootfsAvail, nodeRootfsSize, nodeRootfsUsed,
		nodeUptime, nodeIOWait, nodeLoad1, nodeLoad5, nodeLoad15,
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
	if fp := normalizeFingerprint(st.Info.Fingerprint); fp != "" {
		gauge(ch, nodeInfo, 1, fp)
	}
	if st.CurrentKernel.Release != "" {
		gauge(ch, nodeKernel, 1, st.CurrentKernel.Release)
	}
	optGauge(ch, nodeCPUUsage, st.CPU)
	optGauge(ch, nodeMemoryFree, st.Mem.Free)
	optGauge(ch, nodeMemorySize, st.Mem.Total)
	optGauge(ch, nodeMemoryUsed, st.Mem.Used)
	optGauge(ch, nodeSwapFree, st.Swap.Free)
	optGauge(ch, nodeSwapSize, st.Swap.Total)
	optGauge(ch, nodeSwapUsed, st.Swap.Used)
	optGauge(ch, nodeRootfsAvail, st.Disk.Avail)
	optGauge(ch, nodeRootfsSize, st.Disk.Total)
	optGauge(ch, nodeRootfsUsed, st.Disk.Used)
	optGauge(ch, nodeUptime, st.Uptime)
	optGauge(ch, nodeIOWait, st.Wait)
	// a missing or short loadavg would panic otherwise
	if len(st.Load) >= 3 {
		gauge(ch, nodeLoad1, st.Load[0])
		gauge(ch, nodeLoad5, st.Load[1])
		gauge(ch, nodeLoad15, st.Load[2])
	}
	return nil
}

func collectSubscription(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	var sub map[string]any
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "subscription"), nil, &sub); err != nil {
		return err
	}

	statusStr := ""
	productName := ""
	if v, ok := sub["status"]; ok && v != nil {
		statusStr = fmt.Sprintf("%v", v)
	}
	if v, ok := sub["productname"]; ok && v != nil {
		productName = fmt.Sprintf("%v", v)
	}

	gauge(ch, nodeSubscriptionInfo, 1, productName, statusStr)

	// no series without a due date: 0 would read as 1970 and fire every
	// "subscription expires soon" alert
	if v, ok := sub["nextduedate"].(string); ok {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			gauge(ch, nodeSubscriptionDue, float64(t.Unix()))
		}
	}
	// without Sys.Audit on "/", PBS leaves out the product name and due date
	return s.privilegeGap(ctx, permissions.canReadSubscription, errSubscriptionFiltered)
}
