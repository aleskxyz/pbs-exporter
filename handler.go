package main

import (
	"fmt"
	"html"
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

// exporterMetricsPath serves the exporter's own metrics (go_*, process_*,
// and promhttp_* for the requests of this path), once per exporter instead
// of in every module scrape.
const exporterMetricsPath = "/exporter-metrics"

// newMetricsHandler returns the /metrics handler. Each request selects a
// module with the "module" URL parameter (default "default") and scrapes the
// module's endpoint; other parameters, like "target", are ignored.
//
// Every request gets its own registry, so concurrent scrapes of different
// modules can not see each other's metrics.
func newMetricsHandler(modules map[string]*Module, timeouts scrapeTimeouts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		slog.Debug("scrape", "module", sanitize(moduleName), "endpoint", sanitize(module.api.endpoint), "timeout", timeout)
		// the server has no WriteTimeout, as scrapes may take long; bound the
		// write of this response instead (not supported by test recorders)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + writeMargin))

		reg := prometheus.NewRegistry()
		if err := reg.Register(NewExporter(r.Context(), module, timeout)); err != nil {
			slog.Error("registering the exporter", "err", sanitize(err.Error()))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// a metric that can not be gathered (e.g. a duplicate series) must not
		// cost the whole scrape, pbs_up and the collector status included;
		// the exporter's own metrics are on exporterMetricsPath, so that each
		// module scrape holds only the metrics of its server
		promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
			ErrorLog:      gatherErrorLog{},
		}).ServeHTTP(w, r)
	})
}

// newServeMux returns the routes of the exporter: the module scrapes on the
// metrics path, the exporter's own metrics and a landing page on "/".
func newServeMux(server ServerConfig, modules map[string]*Module) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(server.MetricsPath, newMetricsHandler(modules, server.timeouts()))
	mux.Handle(exporterMetricsPath, promhttp.Handler())
	landingPage := []byte(`<html>
<head><title>PBS Exporter</title></head>
<body>
<h1>Proxmox Backup Server Exporter</h1>
<p><a href='` + html.EscapeString(server.MetricsPath) + `'>Metrics</a></p>
</body>
</html>
`)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// "/" matches every path the other routes do not
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write(landingPage); err != nil {
			slog.Error("failed to write response", "err", sanitize(err.Error()))
		}
	})
	return mux
}
