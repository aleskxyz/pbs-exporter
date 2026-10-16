package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// These variables are set in build step
var Version = "v0.0.0-dev.0"
var Commit = "none"
var BuildTime = "unknown"

var (
	// Flags
	endpoint = flag.String("pbs.endpoint", "",
		"Proxmox Backup Server endpoint")
	username = flag.String("pbs.username", "root@pam",
		"Proxmox Backup Server username")
	apitoken = flag.String("pbs.api.token", "",
		"Proxmox Backup Server API token")
	apitokenname = flag.String("pbs.api.token.name", "pbs-exporter",
		"Proxmox Backup Server API token name")
	timeout = flag.String("pbs.timeout", "5s",
		"Proxmox Backup Server timeout")
	insecure = flag.String("pbs.insecure", "false",
		"Proxmox Backup Server insecure")
	maxConcurrency = flag.String("pbs.max-concurrency", strconv.Itoa(defaultMaxConcurrency),
		"Maximum number of concurrent requests to the Proxmox Backup Server")
	collectorList = flag.String("pbs.collectors", "",
		"Collectors to enable (name) or disable (-name), comma separated, e.g. \"access,-apt\"")
	taskLookback = flag.String("pbs.task-lookback", defaultTaskLookback.String(),
		"How far back the task log of the Proxmox Backup Server is read")
	taskLimit = flag.String("pbs.task-limit", strconv.Itoa(defaultTaskLimit),
		"Maximum number of tasks read from the task log of the Proxmox Backup Server")
	metricsPath = flag.String("pbs.metrics-path", "/metrics",
		"Path under which to expose metrics")
	listenAddress = flag.String("pbs.listen-address", ":10019",
		"Address on which to expose metrics")
	scrapeTimeout = flag.String("pbs.scrape-timeout", "30s",
		"Time limit of a scrape if Prometheus does not send X-Prometheus-Scrape-Timeout-Seconds")
	scrapeTimeoutOffset = flag.String("pbs.scrape-timeout-offset", "500ms",
		"Subtracted from the scrape timeout sent by Prometheus, to leave time for sending the response")
	loglevel = flag.String("pbs.loglevel", "info",
		"Loglevel (debug, info, warn, error)")
	configFile = flag.String("pbs.config-file", "",
		"Path to a YAML file with named modules (credentials per Proxmox Backup Server), selected per scrape with ?module=")
	showVersion = flag.Bool("version", false, "Show version and exit")
)

func ReadSecretFile(secretfilename string) string {
	// #nosec G703 -- path comes from operator-set *_FILE env vars (docker secrets), not untrusted input
	file, err := os.Open(filepath.Clean(secretfilename))
	// flag to check the file format
	if err != nil {
		fatal("unable to open secret file", "err", sanitize(err.Error()))
	}
	// Close the file
	defer func() {
		if err = file.Close(); err != nil {
			fatal("unable to close secret file", "err", sanitize(err.Error()))
		}
	}()
	// Read the first line
	line := bufio.NewScanner(file)
	line.Scan()
	return line.Text()
}

// applyEnv applies the environment variables, which overwrite defaults or flags.
func applyEnv() {
	if os.Getenv("PBS_CONFIG_FILE") != "" {
		*configFile = os.Getenv("PBS_CONFIG_FILE")
	}
	if os.Getenv("PBS_LOGLEVEL") != "" {
		*loglevel = os.Getenv("PBS_LOGLEVEL")
	}
	// with a config file, the connection settings come from the file only and
	// these flags/environment variables are ignored
	if *configFile == "" {
		readConnectionEnv()
	}
	if os.Getenv("PBS_METRICS_PATH") != "" {
		*metricsPath = os.Getenv("PBS_METRICS_PATH")
	}
	if os.Getenv("PBS_LISTEN_ADDRESS") != "" {
		*listenAddress = os.Getenv("PBS_LISTEN_ADDRESS")
	}
	if os.Getenv("PBS_SCRAPE_TIMEOUT") != "" {
		*scrapeTimeout = os.Getenv("PBS_SCRAPE_TIMEOUT")
	}
	if os.Getenv("PBS_SCRAPE_TIMEOUT_OFFSET") != "" {
		*scrapeTimeoutOffset = os.Getenv("PBS_SCRAPE_TIMEOUT_OFFSET")
	}
}

// readConnectionEnv applies the environment variables of the connection
// settings. They are only used without a config file.
func readConnectionEnv() {
	if os.Getenv("PBS_ENDPOINT") != "" {
		*endpoint = os.Getenv("PBS_ENDPOINT")
	}
	if os.Getenv("PBS_USERNAME") != "" {
		*username = os.Getenv("PBS_USERNAME")
	} else {
		if os.Getenv("PBS_USERNAME_FILE") != "" {
			*username = ReadSecretFile(os.Getenv("PBS_USERNAME_FILE"))
		}
	}
	if os.Getenv("PBS_API_TOKEN_NAME") != "" {
		*apitokenname = os.Getenv("PBS_API_TOKEN_NAME")
	} else {
		if os.Getenv("PBS_API_TOKEN_NAME_FILE") != "" {
			*apitokenname = ReadSecretFile(os.Getenv("PBS_API_TOKEN_NAME_FILE"))
		}
	}
	if os.Getenv("PBS_API_TOKEN") != "" {
		*apitoken = os.Getenv("PBS_API_TOKEN")
	} else {
		if os.Getenv("PBS_API_TOKEN_FILE") != "" {
			*apitoken = ReadSecretFile(os.Getenv("PBS_API_TOKEN_FILE"))
		}
	}
	if os.Getenv("PBS_TIMEOUT") != "" {
		*timeout = os.Getenv("PBS_TIMEOUT")
	}
	if os.Getenv("PBS_INSECURE") != "" {
		*insecure = os.Getenv("PBS_INSECURE")
	}
	if os.Getenv("PBS_MAX_CONCURRENCY") != "" {
		*maxConcurrency = os.Getenv("PBS_MAX_CONCURRENCY")
	}
	if os.Getenv("PBS_COLLECTORS") != "" {
		*collectorList = os.Getenv("PBS_COLLECTORS")
	}
	if os.Getenv("PBS_TASK_LOOKBACK") != "" {
		*taskLookback = os.Getenv("PBS_TASK_LOOKBACK")
	}
	if os.Getenv("PBS_TASK_LIMIT") != "" {
		*taskLimit = os.Getenv("PBS_TASK_LIMIT")
	}
}

// parseScrapeTimeouts parses the scrape timeout flags.
func parseScrapeTimeouts() (scrapeTimeouts, error) {
	fallback, err := time.ParseDuration(*scrapeTimeout)
	if err != nil {
		return scrapeTimeouts{}, fmt.Errorf("unable to parse scrape timeout: %w", err)
	}
	if fallback <= 0 {
		return scrapeTimeouts{}, fmt.Errorf("scrape timeout must be greater than 0")
	}
	offset, err := time.ParseDuration(*scrapeTimeoutOffset)
	if err != nil {
		return scrapeTimeouts{}, fmt.Errorf("unable to parse scrape timeout offset: %w", err)
	}
	if offset < 0 {
		return scrapeTimeouts{}, fmt.Errorf("scrape timeout offset must not be negative")
	}
	return scrapeTimeouts{fallback: fallback, offset: offset}, nil
}

// legacyModule builds the "default" module from the flags/environment
// variables, used without a config file.
func legacyModule() (map[string]*Module, error) {
	insecureBool, err := strconv.ParseBool(*insecure)
	if err != nil {
		return nil, fmt.Errorf("unable to parse insecure: %w", err)
	}
	timeoutDuration, err := time.ParseDuration(*timeout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse timeout: %w", err)
	}
	concurrency, err := strconv.Atoi(*maxConcurrency)
	if err != nil || concurrency <= 0 {
		return nil, fmt.Errorf("max concurrency must be a number greater than 0")
	}
	limit, err := strconv.Atoi(*taskLimit)
	if err != nil || limit <= 0 {
		return nil, fmt.Errorf("task limit must be a number greater than 0")
	}
	collectors, err := parseCollectorList(*collectorList)
	if err != nil {
		return nil, fmt.Errorf("collectors: %w", err)
	}

	slog.Debug("legacy module",
		"endpoint", sanitize(*endpoint), "username", sanitize(*username), "token_name", sanitize(*apitokenname),
		"timeout", timeoutDuration, "insecure", insecureBool, "max_concurrency", concurrency)

	return BuildModules(nil, &ModuleConfig{
		User:           *username,
		TokenName:      *apitokenname,
		TokenValue:     *apitoken,
		Endpoint:       *endpoint,
		Insecure:       insecureBool,
		MaxConcurrency: concurrency,
		Collectors:     collectors,
		TaskLookback:   *taskLookback,
		TaskLimit:      limit,
	}, timeoutDuration)
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("PBS Exporter Version: %s, Commit: %s, Build Time: %s\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// if env variable is set, it will overwrite defaults or flags
	applyEnv()
	setupLogging(*loglevel)

	slog.Info("starting PBS Exporter", "version", Version, "commit", Commit, "build_time", BuildTime)

	timeouts, err := parseScrapeTimeouts()
	if err != nil {
		fatal(sanitize(err.Error()))
	}

	var modules map[string]*Module
	if *configFile != "" {
		// modules: named credential sets from the config file
		cfg, m, err := LoadModules(*configFile)
		if err != nil {
			fatal("unable to load config file", "err", sanitize(err.Error()))
		}
		modules = m

		slog.Debug("using config file", "path", sanitize(*configFile))
		for _, name := range moduleNames(modules) {
			mc := cfg[name]
			mod := modules[name]
			var collectors []string
			for _, c := range mod.collectors {
				collectors = append(collectors, c.name)
			}
			slog.Debug("module",
				"name", sanitize(name), "user", sanitize(mc.User), "token_name", sanitize(mc.TokenName),
				"endpoint", sanitize(mc.Endpoint), "timeout", mod.client.Timeout, "insecure", mc.Insecure,
				"max_concurrency", mod.maxConcurrency, "collectors", strings.Join(collectors, ","))
		}
		slog.Info("modules loaded", "modules", sanitize(strings.Join(moduleNames(modules), ", ")))
	} else {
		modules, err = legacyModule()
		if err != nil {
			fatal(sanitize(err.Error()))
		}
		if *endpoint != "" {
			slog.Info("using fixed connection endpoint", "endpoint", sanitize(*endpoint))
		}
	}

	slog.Info("listening", "address", sanitize(*listenAddress), "metrics_path", sanitize(*metricsPath),
		"scrape_timeout", timeouts.fallback, "scrape_timeout_offset", timeouts.offset)

	// start http server
	http.Handle(*metricsPath, newMetricsHandler(modules, *configFile != "", timeouts))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html>
			<head><title>PBS Exporter</title></head>
			<body>
			<h1>Proxmox Backup Server Exporter</h1>
			<p><a href='` + *metricsPath + `'>Metrics</a></p>
			</body>
			</html>`))
		if err != nil {
			slog.Error("failed to write response", "err", sanitize(err.Error()))
		}
	})

	// no WriteTimeout: a scrape may take as long as Prometheus allows (see
	// scrapeTimeouts), the scrape deadline bounds the work instead
	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           nil,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fatal("server stopped", "err", sanitize(server.ListenAndServe().Error()))
}
