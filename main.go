package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
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

// secretsExposed reports whether the config file holds a token_value and can
// be read by group or others.
func secretsExposed(path string, cfg *Config) (bool, error) {
	inline := cfg.Defaults.TokenValue != ""
	for _, m := range cfg.Modules {
		inline = inline || m.TokenValue != ""
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

// shutdownTimeout is how long cancelled scrapes may take to send their
// answer on a stop signal.
const shutdownTimeout = 10 * time.Second

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("PBS Exporter Version: %s, Commit: %s, Build Time: %s\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// the configuration is read on start only; a reload signal must not end
	// the exporter
	signal.Ignore(syscall.SIGHUP)

	cfg, modules, err := LoadConfig(*configFile)
	if err != nil {
		// before logging is set up; the message lists one problem per line
		fmt.Fprintf(os.Stderr, "pbs-exporter: unable to load the config file %s: %s\n", sanitize(*configFile), err)
		os.Exit(1)
	}
	setupLogging(cfg.Server.LogLevel)

	slog.Info("starting PBS Exporter", "version", Version, "commit", Commit, "build_time", BuildTime)
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
			"timeout", mod.api.client.Timeout, "insecure", mod.insecure,
			"max_concurrency", mod.maxConcurrency,
			"collectors", strings.Join(collectors, ","))
	}
	slog.Info("modules loaded", "modules", sanitize(strings.Join(moduleNames(modules), ", ")))

	server := cfg.Server
	timeouts := server.timeouts()
	slog.Info("listening", "address", sanitize(server.ListenAddress), "metrics_path", sanitize(server.MetricsPath),
		"scrape_timeout", timeouts.fallback, "scrape_timeout_offset", timeouts.offset)

	// no WriteTimeout: a scrape may take as long as Prometheus allows (see
	// scrapeTimeouts), the scrape deadline bounds the work instead
	httpServer := &http.Server{
		Handler:           newServeMux(server, modules),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", server.ListenAddress)
	if err != nil {
		fatal("listening", "err", sanitize(err.Error()))
	}
	if err := serve(ctx, httpServer, ln); err != nil {
		fatal("server stopped", "err", sanitize(err.Error()))
	}
}

// serve answers requests on ln until ctx is done. The requests' contexts are
// derived from ctx, so running scrapes are cancelled then and answer with
// what they collected; serve waits up to shutdownTimeout for them.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	srv.BaseContext = func(net.Listener) context.Context { return ctx }
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down, cancelling running scrapes", "timeout", shutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
