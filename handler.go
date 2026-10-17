package main

import (
	"fmt"
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
	if d <= 0 {
		return t.fallback
	}
	if d > t.offset {
		return d - t.offset
	}
	// a timeout shorter than the offset still has to leave time for the response
	return d / 2
}

// writeMargin is how long writing the response may take after the scrape
// timeout; it bounds clients that do not read the response.
const writeMargin = 30 * time.Second

// gatherErrorLog logs the errors of gathering metrics.
type gatherErrorLog struct{}

func (gatherErrorLog) Println(v ...any) {
	slog.Error("gathering metrics", "err", sanitize(fmt.Sprint(v...)))
}

// newMetricsHandler returns the /metrics handler. Each request selects a
// module with the "module" URL parameter (default "default") and scrapes the
// module's endpoint; other parameters, like "target", are ignored.
//
// Every request gets its own registry, so concurrent scrapes of different
// modules can not see each other's metrics.
func newMetricsHandler(modules map[string]*Module, timeouts scrapeTimeouts) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moduleName := r.URL.Query().Get("module")
		if moduleName == "" {
			moduleName = defaultModuleName
		}
		module, ok := modules[moduleName]
		if !ok {
			http.Error(w, "Module not found in config", http.StatusBadRequest)
			return
		}
		timeout := timeouts.scrapeTimeout(r)
		slog.Debug("scrape", "module", sanitize(moduleName), "endpoint", sanitize(module.endpoint), "timeout", timeout)
		// the server has no WriteTimeout, as scrapes may take long; bound the
		// write of this response instead (not supported by test recorders)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + writeMargin))

		reg := prometheus.NewRegistry()
		if err := reg.Register(NewExporter(r.Context(), module, timeout)); err != nil {
			slog.Error("registering the exporter", "err", sanitize(err.Error()))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// also expose the exporter's own go_* / process_* metrics
		gatherers := prometheus.Gatherers{prometheus.DefaultGatherer, reg}
		// a metric that can not be gathered (e.g. a duplicate series) must not
		// cost the whole scrape, pbs_up and the collector status included
		promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
			ErrorLog:      gatherErrorLog{},
		}).ServeHTTP(w, r)
	})
	// same instrumentation as promhttp.Handler(): promhttp_metric_handler_requests_*
	return promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer, h)
}
