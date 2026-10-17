package main

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	aptUpdatesAvailable = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "apt", "updates_available"),
		"Number of packages with a pending update, as of the last package list refresh on the node.",
		nil, nil,
	)
	rebootRequired = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "reboot_required"),
		"Whether a newer kernel than the running one is installed.",
		nil, nil,
	)
)

// kernelPackage matches the versioned kernel packages, e.g.
// "proxmox-kernel-6.8.12-4-pve-signed" (PBS 3+), "pve-kernel-5.15.30-2-pve"
// or the backport "proxmox-kernel-6.14.8-2-bpo12-pve-signed".
var kernelPackage = regexp.MustCompile(`^(?:proxmox|pve)-kernel-(\d+\.\d+\.\d+-\d+(?:-bpo\d+)?)-pve(?:-signed)?$`)

// aptPackage is an entry of /nodes/localhost/apt/{update,versions}.
type aptPackage struct {
	Package string `json:"Package"`
	// OldVersion is the installed version.
	OldVersion string `json:"OldVersion"`
}

// compareKernelVersions compares versions like "6.8.12-4" numerically.
func compareKernelVersions(a, b string) int {
	split := func(v string) []string {
		return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' })
	}
	pa, pb := split(a), split(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// newestInstalledKernel returns the version of the newest installed
// versioned kernel package, "" if there is none.
func newestInstalledKernel(packages []aptPackage) string {
	newest := ""
	for _, p := range packages {
		m := kernelPackage.FindStringSubmatch(p.Package)
		if m == nil || p.OldVersion == "" {
			continue
		}
		if newest == "" || compareKernelVersions(m[1], newest) > 0 {
			newest = m[1]
		}
	}
	return newest
}

var aptCollector = &collector{
	name:      "apt",
	enabled:   true,
	privilege: "Sys.Audit on /",
	descs:     []*prometheus.Desc{aptUpdatesAvailable, rebootRequired},
	collect: func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
		return errors.Join(collectAptUpdates(ctx, s, ch), collectRebootRequired(ctx, s, ch))
	},
}

func collectAptUpdates(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	// GET lists the updates known to the node, it does not refresh the list
	var updates []aptPackage
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "apt", "update"), nil, &updates); err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(aptUpdatesAvailable, prometheus.GaugeValue, float64(len(updates)))
	return nil
}

func collectRebootRequired(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error {
	st, err := s.getNodeStatus(ctx)
	if err != nil {
		// the node collector reports it; without the running kernel there
		// is nothing to compare
		return nil
	}
	var versions []aptPackage
	if err := s.api.get(ctx, apiPath("nodes", "localhost", "apt", "versions"), nil, &versions); err != nil {
		return err
	}
	running := strings.TrimSuffix(st.CurrentKernel.Release, "-pve")
	newest := newestInstalledKernel(versions)
	// without both versions there is nothing to compare
	if running == "" || newest == "" {
		return nil
	}
	required := 0.0
	if compareKernelVersions(newest, running) > 0 {
		required = 1
	}
	ch <- prometheus.MustNewConstMetric(rebootRequired, prometheus.GaugeValue, required)
	return nil
}
