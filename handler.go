package main

import (
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func moduleNames(modules map[string]*Module) []string {
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// scrapeTimeouts bound the time of a scrape.
type scrapeTimeouts struct {
	// fallback is used when the client sends no scrape timeout.
	fallback time.Duration
	// offset is subtracted from the scrape timeout sent by Prometheus, so
	// the response arrives before Prometheus gives up.
	offset time.Duration
}

// scrapeTimeout returns how long a scrape may collect: the timeout Prometheus
// sends in X-Prometheus-Scrape-Timeout-Seconds minus the offset, or the
// fallback without the header.
func (t scrapeTimeouts) scrapeTimeout(r *http.Request) time.Duration {
	v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds")
	if v == "" {
		return t.fallback
	}
	secs, err := strconv.ParseFloat(v, 64)
	// also refuses values that do not fit into a time.Duration
	if err != nil || !(secs > 0 && secs < math.MaxInt64/float64(time.Second)) {
		return t.fallback
	}
	d := time.Duration(secs * float64(time.Second))
	if d > t.offset {
		d -= t.offset
	}
	return d
}

// newMetricsHandler returns the /metrics handler. With a config file
// (configMode), each request selects a module with the "module" URL parameter
// (default "default") and scrapes the module's endpoint. Without one, the
// "module" parameter is ignored, as it was before modules existed, the
// "default" module is always used, and unless pbs.endpoint pins it, the Proxmox
// Backup Server to scrape is taken from the "target" URL parameter.
//
// Every request gets its own registry, so concurrent scrapes of different
// targets can not see each other's metrics.
func newMetricsHandler(modules map[string]*Module, configMode bool, timeouts scrapeTimeouts) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moduleName := defaultModuleName
		if configMode && r.URL.Query().Get("module") != "" {
			moduleName = r.URL.Query().Get("module")
		}
		module, ok := modules[moduleName]
		if !ok {
			http.Error(w, "Module not found in config", http.StatusBadRequest)
			return
		}

		// a module with a fixed endpoint (always the case for config file
		// modules) ignores the "target" parameter, otherwise it is taken from
		// the "target" query parameter
		target := module.endpoint
		if target == "" {
			target = r.URL.Query().Get("target")
			if target == "" {
				// if target is not set, we use the default
				target = "http://localhost:8007"
			}
		}

		timeout := timeouts.scrapeTimeout(r)
		slog.Debug("scrape", "module", sanitize(moduleName), "endpoint", sanitize(target), "timeout", timeout)

		reg := prometheus.NewRegistry()
		if err := reg.Register(NewExporter(r.Context(), target, module, timeout)); err != nil {
			slog.Error("registering the exporter", "err", sanitize(err.Error()))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// also expose the exporter's own go_* / process_* metrics
		gatherers := prometheus.Gatherers{prometheus.DefaultGatherer, reg}
		promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
	// same instrumentation as promhttp.Handler(): promhttp_metric_handler_requests_*
	return promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer, h)
}
