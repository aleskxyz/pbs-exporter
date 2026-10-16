package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sync/semaphore"
)

const defaultModuleName = "default"

// Defaults of the server settings.
const (
	defaultListenAddress       = ":10019"
	defaultMetricsPath         = "/metrics"
	defaultLogLevel            = "info"
	defaultScrapeTimeout       = 30 * time.Second
	defaultScrapeTimeoutOffset = 500 * time.Millisecond
)

// defaultModuleTimeout is the timeout of requests of a module that sets none.
const defaultModuleTimeout = 5 * time.Second

// defaultMaxConcurrency is the number of concurrent requests to one PBS of a
// module that sets none.
const defaultMaxConcurrency = 4

// defaultTaskLookback is how far back the task log is read by default; long
// enough to see the last run of a weekly job.
const defaultTaskLookback = 8 * 24 * time.Hour

// defaultTaskLimit is the default maximum number of tasks read from the task log.
const defaultTaskLimit = 10000

// Config is the content of the config file.
type Config struct {
	Server ServerConfig `yaml:"server"`
	// Defaults are module settings that apply to every module that does not
	// set them itself.
	Defaults ModuleConfig `yaml:"defaults"`
	// Modules are the Proxmox Backup Servers, selected per scrape with the
	// "module" URL parameter. After ParseConfig, they are merged with
	// Defaults and validated.
	Modules map[string]ModuleConfig `yaml:"modules"`
}

// ServerConfig are the settings of the exporter's HTTP server. After
// ParseConfig, every field is set.
type ServerConfig struct {
	ListenAddress string `yaml:"listen_address"`
	MetricsPath   string `yaml:"metrics_path"`
	// LogLevel is debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
	// ScrapeTimeout limits a scrape if Prometheus sends no
	// X-Prometheus-Scrape-Timeout-Seconds header.
	ScrapeTimeout *Duration `yaml:"scrape_timeout"`
	// ScrapeTimeoutOffset is subtracted from the timeout Prometheus sends, to
	// leave time for sending the response.
	ScrapeTimeoutOffset *Duration `yaml:"scrape_timeout_offset"`
}

// timeouts returns the scrape timeouts of the server settings.
func (s ServerConfig) timeouts() scrapeTimeouts {
	return scrapeTimeouts{fallback: time.Duration(*s.ScrapeTimeout), offset: time.Duration(*s.ScrapeTimeoutOffset)}
}

// ModuleConfig holds the credentials and connection settings used to talk to a
// Proxmox Backup Server. Settings that are not set (nil, "") are taken from
// the defaults section, then from the built-in defaults.
type ModuleConfig struct {
	// Endpoint is the PBS the module scrapes (required, only per module).
	// Redirects are not followed, so the token is only sent to it.
	Endpoint string `yaml:"endpoint"`
	// User is the PBS user the API token belongs to, e.g. "monitoring@pbs".
	User string `yaml:"user"`
	// TokenName is the name of the API token.
	TokenName string `yaml:"token_name"`
	// TokenValue is the secret of the API token. Either it or TokenValueFile
	// is required.
	TokenValue string `yaml:"token_value"`
	// TokenValueFile is a file that holds the secret of the API token (e.g. a
	// Docker or Kubernetes secret); it is read on start.
	TokenValueFile string `yaml:"token_value_file"`
	// Insecure disables TLS certificate verification.
	Insecure *bool `yaml:"insecure"`
	// Timeout of each request to the PBS, defaults to defaultModuleTimeout.
	Timeout *Duration `yaml:"timeout"`
	// MaxConcurrency limits the concurrent requests to the PBS, defaults to
	// defaultMaxConcurrency.
	MaxConcurrency *int `yaml:"max_concurrency"`
	// Collectors enables (true) or disables (false) collectors; collectors
	// that are not listed keep their default.
	Collectors map[string]bool `yaml:"collectors"`
	// TaskLookback is how far back the task log is read, defaults to
	// defaultTaskLookback.
	TaskLookback *Duration `yaml:"task_lookback"`
	// TaskLimit is the maximum number of tasks read from the task log,
	// defaults to defaultTaskLimit.
	TaskLimit *int `yaml:"task_limit"`
}

// Duration is a time.Duration written as Go duration ("15s", "192h").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// Module is a validated ModuleConfig with a dedicated HTTP client, so that
// TLS and timeout settings are not shared between modules.
type Module struct {
	name                string
	endpoint            string
	authorizationHeader string
	client              *http.Client
	maxConcurrency      int
	// sem limits the concurrent requests of all scrapes of the module.
	sem          *semaphore.Weighted
	collectors   []*collector
	taskLookback time.Duration
	taskLimit    int
}

// LoadConfig reads the config file and builds its modules.
func LoadConfig(path string) (*Config, map[string]*Module, error) {
	// the path is logged, also as part of the read error
	if hasControlChar(path) {
		return nil, nil, errors.New("config file path must not contain control characters")
	}
	// #nosec G304 G703 -- path is set by the operator via flag, not untrusted input
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, nil, fmt.Errorf("unable to read config file: %w", err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, nil, err
	}
	modules, err := BuildModules(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cfg, modules, nil
}

// ParseConfig parses and validates the YAML content of a config file and
// merges the modules with the defaults.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid config file: %w", err)
	}
	// only one YAML document is read, so refuse others instead of ignoring
	// them; an empty trailing document (e.g. a final "---") is fine
	for {
		var extra yaml.Node
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || !isEmptyDocument(&extra) {
			return nil, fmt.Errorf("invalid config file: only one YAML document is allowed")
		}
	}
	// a null module name ("~:" or "null:") is silently dropped by Decode
	if hasNullModuleName(data) {
		return nil, fmt.Errorf("invalid config file: module name must not be empty or null")
	}
	return NewConfig(cfg.Server, cfg.Defaults, cfg.Modules)
}

// NewConfig validates the settings of a config file, fills in the server
// defaults and merges every module with the defaults.
func NewConfig(server ServerConfig, defaults ModuleConfig, modules map[string]ModuleConfig) (*Config, error) {
	var invalid []string
	add := func(format string, args ...any) {
		invalid = append(invalid, "  - "+fmt.Sprintf(format, args...))
	}

	for _, msg := range validateServer(&server) {
		add("server: %s", msg)
	}

	if defaults.Endpoint != "" {
		add("defaults: endpoint must be set per module")
	}
	for _, msg := range validateModuleFields(defaults) {
		add("defaults: %s", msg)
	}

	if len(modules) == 0 {
		add("modules: no modules specified")
	}
	merged := map[string]ModuleConfig{}
	for name, m := range modules {
		switch {
		case name == "":
			add("module name must not be empty")
			continue
		case hasControlChar(name):
			// module names are logged; the name itself is not part of the message
			add("module name must not contain control characters")
			continue
		}
		for _, msg := range validateModuleFields(m) {
			add("%s: %s", name, msg)
		}
		m = mergeModule(defaults, m)
		if m.Endpoint == "" {
			add("%s: endpoint is required", name)
		} else if endpoint, err := validateBaseURL(m.Endpoint); err != nil {
			add("%s: endpoint %s", name, err)
		} else {
			m.Endpoint = endpoint
		}
		if m.User == "" || m.TokenName == "" {
			add("%s: user and token_name are required (in the module or in defaults)", name)
		}
		if m.TokenValue == "" && m.TokenValueFile == "" {
			add("%s: token_value or token_value_file is required (in the module or in defaults)", name)
		}
		merged[name] = m
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, fmt.Errorf("invalid config file:\n%s", strings.Join(invalid, "\n"))
	}
	return &Config{Server: server, Defaults: defaults, Modules: merged}, nil
}

// validateServer checks the server settings and fills in their defaults.
func validateServer(s *ServerConfig) []string {
	var invalid []string
	if s.ListenAddress == "" {
		s.ListenAddress = defaultListenAddress
	}
	if s.MetricsPath == "" {
		s.MetricsPath = defaultMetricsPath
	} else if !strings.HasPrefix(s.MetricsPath, "/") || hasControlChar(s.MetricsPath) {
		invalid = append(invalid, "metrics_path must be a path starting with /")
	}
	if s.LogLevel == "" {
		s.LogLevel = defaultLogLevel
	} else if _, err := parseLogLevel(s.LogLevel); err != nil {
		invalid = append(invalid, "log_level must be debug, info, warn or error")
	}
	if s.ScrapeTimeout == nil {
		s.ScrapeTimeout = ptr(Duration(defaultScrapeTimeout))
	} else if *s.ScrapeTimeout <= 0 {
		invalid = append(invalid, "scrape_timeout must be greater than 0")
	}
	if s.ScrapeTimeoutOffset == nil {
		s.ScrapeTimeoutOffset = ptr(Duration(defaultScrapeTimeoutOffset))
	} else if *s.ScrapeTimeoutOffset < 0 {
		invalid = append(invalid, "scrape_timeout_offset must not be negative")
	}
	return invalid
}

// validateModuleFields checks the settings a module or the defaults set
// themselves; required settings are checked after merging.
func validateModuleFields(m ModuleConfig) []string {
	var invalid []string
	if hasControlChar(m.User) || hasControlChar(m.TokenName) || hasControlChar(m.TokenValue) {
		invalid = append(invalid, "user, token_name and token_value must not contain control characters")
	}
	if m.TokenValue != "" && m.TokenValueFile != "" {
		invalid = append(invalid, "only one of token_value and token_value_file may be set")
	}
	if hasControlChar(m.TokenValueFile) {
		invalid = append(invalid, "token_value_file must not contain control characters")
	}
	if m.Timeout != nil && *m.Timeout <= 0 {
		invalid = append(invalid, "timeout must be greater than 0")
	}
	if m.MaxConcurrency != nil && *m.MaxConcurrency <= 0 {
		invalid = append(invalid, "max_concurrency must be greater than 0")
	}
	if m.TaskLookback != nil && *m.TaskLookback <= 0 {
		invalid = append(invalid, "task_lookback must be greater than 0")
	}
	if m.TaskLimit != nil && *m.TaskLimit <= 0 {
		invalid = append(invalid, "task_limit must be greater than 0")
	}
	if err := validateCollectors(m.Collectors); err != nil {
		invalid = append(invalid, "collectors: "+err.Error())
	}
	return invalid
}

// mergeModule returns m with the settings it does not set taken from
// defaults. The token is taken as a whole: a module that sets token_value or
// token_value_file uses neither of the defaults.
func mergeModule(defaults, m ModuleConfig) ModuleConfig {
	if m.User == "" {
		m.User = defaults.User
	}
	if m.TokenName == "" {
		m.TokenName = defaults.TokenName
	}
	if m.TokenValue == "" && m.TokenValueFile == "" {
		m.TokenValue, m.TokenValueFile = defaults.TokenValue, defaults.TokenValueFile
	}
	m.Insecure = orDefault(m.Insecure, defaults.Insecure)
	m.Timeout = orDefault(m.Timeout, defaults.Timeout)
	m.MaxConcurrency = orDefault(m.MaxConcurrency, defaults.MaxConcurrency)
	m.TaskLookback = orDefault(m.TaskLookback, defaults.TaskLookback)
	m.TaskLimit = orDefault(m.TaskLimit, defaults.TaskLimit)
	if len(defaults.Collectors) > 0 {
		collectors := map[string]bool{}
		for name, on := range defaults.Collectors {
			collectors[name] = on
		}
		for name, on := range m.Collectors {
			collectors[name] = on
		}
		m.Collectors = collectors
	}
	return m
}

func orDefault[T any](v, d *T) *T {
	if v != nil {
		return v
	}
	return d
}

func ptr[T any](v T) *T {
	return &v
}

// readTokenFile reads the token secret from a file: its content without a
// trailing line break.
func readTokenFile(path string) (string, error) {
	// #nosec G304 G703 -- path is set by the operator in the config file, not untrusted input
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	// the errors must not contain the content, it is a secret
	switch {
	case value == "":
		return "", errors.New("the file is empty")
	case hasControlChar(value):
		return "", errors.New("the token must not contain control characters (other than one final line break)")
	}
	return value, nil
}

// isEmptyDocument reports whether n is a YAML document without content.
func isEmptyDocument(n *yaml.Node) bool {
	if n.Kind != yaml.DocumentNode {
		return false
	}
	for _, c := range n.Content {
		if c.Kind != yaml.ScalarNode || c.Tag != "!!null" {
			return false
		}
	}
	return true
}

// hasNullModuleName reports whether the modules mapping has a null key.
func hasNullModuleName(data []byte) bool {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) == 0 {
		return false
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "modules" || top.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		modules := top.Content[i+1]
		for j := 0; j < len(modules.Content); j += 2 {
			if k := modules.Content[j]; k.Kind == yaml.ScalarNode && k.Tag == "!!null" {
				return true
			}
		}
	}
	return false
}

// hasControlChar reports whether s contains an ASCII control character, e.g.
// the trailing newline of a YAML block scalar ("token_value: |").
func hasControlChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// validateBaseURL checks that s is a base URL like "https://host:8007" (an
// optional trailing "/" is allowed) and returns it without the trailing "/".
// The API paths are appended to it, so a path, query or fragment would change
// which URL is requested.
func validateBaseURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("must use the http or https scheme")
	case u.Opaque != "" || u.Hostname() == "":
		return "", errors.New("must contain a host")
	case u.User != nil:
		return "", errors.New("must not contain user info")
	case u.Path != "" && u.Path != "/":
		return "", errors.New("must not contain a path")
	case u.RawQuery != "" || u.ForceQuery:
		return "", errors.New("must not contain a query")
	case u.Fragment != "" || strings.Contains(s, "#"):
		return "", errors.New("must not contain a fragment")
	}
	return strings.TrimSuffix(s, "/"), nil
}

// BuildModules reads the token files and builds the modules of a parsed
// config.
func BuildModules(cfg *Config) (map[string]*Module, error) {
	modules := map[string]*Module{}
	var invalid []string
	for name, mc := range cfg.Modules {
		if mc.TokenValueFile != "" {
			value, err := readTokenFile(mc.TokenValueFile)
			if err != nil {
				invalid = append(invalid, fmt.Sprintf("  - %s: token_value_file: %s", name, err))
				continue
			}
			mc.TokenValue = value
		}
		modules[name] = NewModule(name, mc)
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, fmt.Errorf("invalid config file:\n%s", strings.Join(invalid, "\n"))
	}
	return modules, nil
}

// NewModule builds a Module from a validated module config with its token.
func NewModule(name string, cfg ModuleConfig) *Module {
	timeout := durationOr(cfg.Timeout, defaultModuleTimeout)
	maxConcurrency := intOr(cfg.MaxConcurrency, defaultMaxConcurrency)

	// the default transport brings dial, TLS handshake and idle timeouts
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// no proxy from the environment
	tr.Proxy = nil
	// HTTP/1.1: a custom TLS config disabled HTTP/2 before, keep it that way
	tr.ForceAttemptHTTP2 = false
	tr.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// #nosec G402 -- opt-in per module
		InsecureSkipVerify: cfg.Insecure != nil && *cfg.Insecure,
	}
	tr.MaxIdleConnsPerHost = maxConcurrency
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// do not follow redirects, so the token is never sent to another host
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Module{
		name:                name,
		endpoint:            cfg.Endpoint,
		authorizationHeader: "PBSAPIToken=" + cfg.User + "!" + cfg.TokenName + ":" + cfg.TokenValue,
		client:              client,
		maxConcurrency:      maxConcurrency,
		sem:                 semaphore.NewWeighted(int64(maxConcurrency)),
		collectors:          enabledCollectors(cfg.Collectors),
		taskLookback:        durationOr(cfg.TaskLookback, defaultTaskLookback),
		taskLimit:           intOr(cfg.TaskLimit, defaultTaskLimit),
	}
}

func durationOr(v *Duration, d time.Duration) time.Duration {
	if v != nil {
		return time.Duration(*v)
	}
	return d
}

func intOr(v *int, d int) int {
	if v != nil {
		return *v
	}
	return d
}
