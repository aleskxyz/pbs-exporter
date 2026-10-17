package main

import (
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// These variables are set in build step
var Version = "v0.0.0-dev.0"
var Commit = "none"
var BuildTime = "unknown"

const defaultConfigFile = "/etc/pbs-exporter/config.yml"

var (
	configFile = flag.String("config.file", defaultConfigFile,
		"Path to the YAML config file (server settings, module defaults and one module per Proxmox Backup Server)")
	showVersion = flag.Bool("version", false, "Show version and exit")
)

// removedEnv are the environment variables of earlier versions. The
// configuration is only read from the config file now; a set variable is
// reported, so the change does not go unnoticed.
var removedEnv = []string{
	"PBS_CONFIG_FILE", "PBS_ENDPOINT", "PBS_USERNAME", "PBS_USERNAME_FILE",
	"PBS_API_TOKEN", "PBS_API_TOKEN_FILE", "PBS_API_TOKEN_NAME", "PBS_API_TOKEN_NAME_FILE",
	"PBS_TIMEOUT", "PBS_INSECURE", "PBS_MAX_CONCURRENCY", "PBS_COLLECTORS",
	"PBS_TASK_LOOKBACK", "PBS_TASK_LIMIT", "PBS_METRICS_PATH", "PBS_LISTEN_ADDRESS",
	"PBS_SCRAPE_TIMEOUT", "PBS_SCRAPE_TIMEOUT_OFFSET", "PBS_LOGLEVEL",
}

// secretsExposed reports whether the config file holds a token_value and can
// be read by group or others.
func secretsExposed(path string, cfg *Config) (bool, error) {
	inline := cfg.Defaults.TokenValue != ""
	for _, m := range cfg.Modules {
		inline = inline || (m.TokenValue != "" && m.TokenValueFile == "")
	}
	if !inline {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 != 0, nil
}

// legacyEnvSet returns the removed environment variables that are set.
func legacyEnvSet() []string {
	var set []string
	for _, name := range removedEnv {
		if _, ok := os.LookupEnv(name); ok {
			set = append(set, name)
		}
	}
	return set
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("PBS Exporter Version: %s, Commit: %s, Build Time: %s\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// the configuration is read on start only; a reload signal must not end
	// the exporter
	signal.Ignore(syscall.SIGHUP)

	legacyEnv := legacyEnvSet()
	cfg, modules, err := LoadConfig(*configFile)
	if err != nil {
		// before logging is set up; the message lists one problem per line
		fmt.Fprintf(os.Stderr, "pbs-exporter: unable to load the config file %s: %s\n", sanitize(*configFile), err)
		if len(legacyEnv) > 0 {
			// most likely an update from a version configured by environment variables
			fmt.Fprintf(os.Stderr, "pbs-exporter: the environment variables %s are not used anymore, see pbs-exporter.example.yml\n", strings.Join(legacyEnv, ", "))
		}
		os.Exit(1)
	}
	setupLogging(cfg.Server.LogLevel)

	slog.Info("starting PBS Exporter", "version", Version, "commit", Commit, "build_time", BuildTime)
	if len(legacyEnv) > 0 {
		slog.Warn("environment variables are not used anymore, the configuration is read from the config file only",
			"variables", strings.Join(legacyEnv, ","), "config_file", sanitize(*configFile))
	}
	if exposed, err := secretsExposed(*configFile, cfg); err == nil && exposed {
		slog.Warn("the config file holds API tokens and is readable by other users, restrict it to the exporter's user (e.g. chmod 600)",
			"config_file", sanitize(*configFile))
	}

	for _, name := range moduleNames(modules) {
		mc, mod := cfg.Modules[name], modules[name]
		var collectors []string
		for _, c := range mod.collectors {
			collectors = append(collectors, c.name)
		}
		slog.Debug("module",
			"name", sanitize(name), "endpoint", sanitize(mc.Endpoint), "user", sanitize(mc.User),
			"token_name", sanitize(mc.TokenName), "token_from_file", mc.TokenValueFile != "",
			"timeout", mod.client.Timeout, "insecure", mod.insecure,
			"max_concurrency", mod.maxConcurrency, "task_lookback", mod.taskLookback, "task_limit", mod.taskLimit,
			"collectors", strings.Join(collectors, ","))
	}
	slog.Info("modules loaded", "modules", sanitize(strings.Join(moduleNames(modules), ", ")))

	server := cfg.Server
	timeouts := server.timeouts()
	slog.Info("listening", "address", sanitize(server.ListenAddress), "metrics_path", sanitize(server.MetricsPath),
		"scrape_timeout", timeouts.fallback, "scrape_timeout_offset", timeouts.offset)

	http.Handle(server.MetricsPath, newMetricsHandler(modules, timeouts))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html>
			<head><title>PBS Exporter</title></head>
			<body>
			<h1>Proxmox Backup Server Exporter</h1>
			<p><a href='` + html.EscapeString(server.MetricsPath) + `'>Metrics</a></p>
			</body>
			</html>`))
		if err != nil {
			slog.Error("failed to write response", "err", sanitize(err.Error()))
		}
	})

	// no WriteTimeout: a scrape may take as long as Prometheus allows (see
	// scrapeTimeouts), the scrape deadline bounds the work instead
	httpServer := &http.Server{
		Addr:              server.ListenAddress,
		Handler:           nil,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fatal("server stopped", "err", sanitize(httpServer.ListenAndServe().Error()))
}
