package main

import (
	"log"
	"net/http"
	"sort"
	"strings"

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

// newMetricsHandler returns the /metrics handler. With a config file
// (configMode), each request selects a module with the "module" URL parameter
// (default "default") and scrapes the module's endpoint. Without one, the
// "module" parameter is ignored, as it was before modules existed, the
// "default" module is always used, and unless pbs.endpoint pins it, the Proxmox
// Backup Server to scrape is taken from the "target" URL parameter.
//
// Every request gets its own registry, so concurrent scrapes of different
// targets can not see each other's metrics.
func newMetricsHandler(modules map[string]*Module, debug bool, configMode bool) http.Handler {
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

		if debug && configMode {
			log.Printf("DEBUG: Using module %s, connection endpoint %s",
				strings.ReplaceAll(strings.ReplaceAll(moduleName, "\n", ""), "\r", ""),
				strings.ReplaceAll(strings.ReplaceAll(target, "\n", ""), "\r", ""))
		} else if debug {
			log.Printf("DEBUG: Using connection endpoint %s", strings.ReplaceAll(strings.ReplaceAll(target, "\n", ""), "\r", ""))
		}

		reg := prometheus.NewRegistry()
		if err := reg.Register(NewExporter(r.Context(), target, module)); err != nil {
			log.Printf("ERROR: %s", err)
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
