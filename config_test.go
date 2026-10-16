package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// validModule is a module body (indented for "modules:") without errors, so
// that each invalid case below is invalid for one reason only.
const validModule = "    endpoint: https://pbs:8007\n    user: u@pbs\n    token_name: n\n    token_value: v\n"

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
server:
  listen_address: 127.0.0.1:9000
  metrics_path: /pbs
  log_level: debug
  scrape_timeout: 1m
  scrape_timeout_offset: 0s
defaults:
  user: monitoring@pbs
  token_name: exporter
  token_value_file: /run/secrets/pbs
  timeout: 15s
  insecure: true
  max_concurrency: 2
  collectors:
    access: true
    apt: false
modules:
  dc1:
    endpoint: https://10.0.0.1:8007/
  fra3:
    endpoint: https://pbs.example.org:8007
    user: other@pbs
    token_value: secret
    insecure: false
    task_lookback: 48h
    task_limit: 500
    collectors:
      apt: true
      disks: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Server; s.ListenAddress != "127.0.0.1:9000" || s.MetricsPath != "/pbs" || s.LogLevel != "debug" ||
		time.Duration(*s.ScrapeTimeout) != time.Minute || *s.ScrapeTimeoutOffset != 0 {
		t.Errorf("server = %+v", s)
	}

	dc1 := cfg.Modules["dc1"]
	if dc1.Endpoint != "https://10.0.0.1:8007" || dc1.User != "monitoring@pbs" || dc1.TokenName != "exporter" ||
		dc1.TokenValueFile != "/run/secrets/pbs" || dc1.TokenValue != "" || !*dc1.Insecure ||
		time.Duration(*dc1.Timeout) != 15*time.Second || *dc1.MaxConcurrency != 2 || dc1.TaskLookback != nil {
		t.Errorf("dc1 = %+v", dc1)
	}
	if !dc1.Collectors["access"] || dc1.Collectors["apt"] || len(dc1.Collectors) != 2 {
		t.Errorf("dc1 collectors = %v", dc1.Collectors)
	}

	// settings of the module win; the token is replaced as a whole
	fra3 := cfg.Modules["fra3"]
	if fra3.User != "other@pbs" || fra3.TokenName != "exporter" || fra3.TokenValue != "secret" || fra3.TokenValueFile != "" ||
		*fra3.Insecure || time.Duration(*fra3.TaskLookback) != 48*time.Hour || *fra3.TaskLimit != 500 {
		t.Errorf("fra3 = %+v", fra3)
	}
	if !fra3.Collectors["access"] || !fra3.Collectors["apt"] || !fra3.Collectors["disks"] {
		t.Errorf("fra3 collectors = %v, want the defaults merged with its own", fra3.Collectors)
	}
}

// Without server settings and defaults, the built-in defaults apply.
func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("modules:\n  site:\n" + validModule))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Server; s.ListenAddress != ":10019" || s.MetricsPath != "/metrics" || s.LogLevel != "info" ||
		s.timeouts() != (scrapeTimeouts{fallback: 30 * time.Second, offset: 500 * time.Millisecond}) {
		t.Errorf("server = %+v", s)
	}
	modules, err := BuildModules(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := modules["site"]
	if m.authorizationHeader != "PBSAPIToken=u@pbs!n:v" || m.endpoint != "https://pbs:8007" {
		t.Errorf("unexpected module: %+v", m)
	}
	if m.client.Timeout != defaultModuleTimeout || m.maxConcurrency != defaultMaxConcurrency ||
		m.taskLookback != defaultTaskLookback || m.taskLimit != defaultTaskLimit {
		t.Errorf("module defaults: timeout %s, max_concurrency %d, task_lookback %s, task_limit %d",
			m.client.Timeout, m.maxConcurrency, m.taskLookback, m.taskLimit)
	}
	if m.client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Error("TLS must be verified unless insecure is set")
	}
	if m.client.CheckRedirect == nil {
		t.Error("redirects must not be followed")
	}
	var names []string
	for _, c := range m.collectors {
		names = append(names, c.name)
	}
	if slices.Contains(names, "access") || slices.Contains(names, "disks") || !slices.Contains(names, "groups") {
		t.Errorf("default collectors %v", names)
	}
}

func TestParseConfigErrors(t *testing.T) {
	const mod = "modules:\n  a:\n" + validModule
	for name, in := range map[string]string{
		"empty":                ``,
		"not a map":            `- a`,
		"no modules":           "server:\n  log_level: info\n",
		"old format":           "a:\n  user: u\n  token_name: n\n  token_value: v\n  endpoint: https://pbs:8007\n",
		"missing token":        "modules:\n  a:\n    endpoint: https://pbs:8007\n    user: u\n    token_name: n\n",
		"missing user":         "modules:\n  a:\n    endpoint: https://pbs:8007\n    token_name: n\n    token_value: v\n",
		"missing endpoint":     "modules:\n  a:\n    user: u\n    token_name: n\n    token_value: v\n",
		"endpoint in defaults": "defaults:\n  endpoint: https://pbs:8007\n" + mod,
		"two token sources":    mod + "    token_value_file: /run/secrets/a\n",
		"defaults two tokens":  "defaults:\n  token_value: v\n  token_value_file: /f\n" + mod,
		"unknown module key":   mod + "    verify_ssl: false\n",
		"unknown server key":   "server:\n  port: 1\n" + mod,
		"unknown section":      "global:\n  x: 1\n" + mod,
		"bad log level":        "server:\n  log_level: verbose\n" + mod,
		"bad metrics path":     "server:\n  metrics_path: metrics\n" + mod,
		"zero scrape timeout":  "server:\n  scrape_timeout: 0s\n" + mod,
		"neg offset":           "server:\n  scrape_timeout_offset: -1s\n" + mod,
		"bad duration":         mod + "    timeout: soon\n",
		"zero timeout":         mod + "    timeout: 0s\n",
		"zero in defaults":     "defaults:\n  timeout: 0s\n" + mod,
		"zero concurrency":     mod + "    max_concurrency: 0\n",
		"neg task limit":       mod + "    task_limit: -1\n",
		"day duration":         mod + "    task_lookback: 8d\n",
		"unknown collector":    mod + "    collectors:\n      nope: true\n",
		"disabled version":     "defaults:\n  collectors:\n    version: false\n" + mod,
		"collectors list":      mod + "    collectors: [node]\n",
		"bad endpoint":         "modules:\n  a:\n    endpoint: https://pbs:8007/api2\n    user: u\n    token_name: n\n    token_value: v\n",
		"two documents":        mod + "---\nmodules:\n  b:\n" + validModule,
		"duplicate module":     mod + "  a:\n" + validModule,
		"empty name":           "modules:\n  \"\":\n" + validModule,
		"null name":            "modules:\n  ~:\n" + validModule,
		"null word name":       "modules:\n  null:\n" + validModule,
		"newline name":         "modules:\n  \"a\\nINFO: forged\":\n" + validModule,
		"block token":          "modules:\n  a:\n    endpoint: https://pbs:8007\n    user: u\n    token_name: n\n    token_value: |\n      v\n",
		"tab user":             "modules:\n  a:\n    endpoint: https://pbs:8007\n    user: \"u\\tx\"\n    token_name: n\n    token_value: v\n",
	} {
		if _, err := ParseConfig([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// an empty trailing document is fine
	for _, in := range []string{mod + "---\n", "---\n" + mod + "---\n...\n"} {
		if _, err := ParseConfig([]byte(in)); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// All errors of a config file are reported at once, attributed to the
// section they are in.
func TestParseConfigReportsAllErrors(t *testing.T) {
	_, err := ParseConfig([]byte("server:\n  log_level: loud\ndefaults:\n  timeout: 0s\nmodules:\n  a:\n    user: u\n  b:\n" + validModule + "    task_limit: 0\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"server: log_level must be",
		"defaults: timeout must be greater than 0",
		"a: endpoint is required",
		"a: token_value or token_value_file is required",
		"b: task_limit must be greater than 0",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, err)
		}
	}
}

func TestTokenValueFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	build := func(file string) (map[string]*Module, error) {
		cfg, err := ParseConfig([]byte("defaults:\n  user: u@pbs\n  token_name: n\n  token_value_file: " + file + "\nmodules:\n  a:\n    endpoint: https://pbs:8007\n"))
		if err != nil {
			t.Fatal(err)
		}
		return BuildModules(cfg)
	}

	for content, want := range map[string]string{"secret": "secret", "secret\n": "secret", "secret\r\n": "secret"} {
		modules, err := build(write("token", content))
		if err != nil {
			t.Fatalf("%q: %v", content, err)
		}
		if got := modules["a"].authorizationHeader; got != "PBSAPIToken=u@pbs!n:"+want {
			t.Errorf("%q: Authorization = %q", content, got)
		}
	}

	for name, content := range map[string]string{"empty": "", "two lines": "sec\nret\n", "tab": "sec\tret"} {
		_, err := build(write("bad", content))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if content != "" && strings.Contains(err.Error(), "sec") {
			t.Errorf("%s: the error must not contain the secret: %v", name, err)
		}
	}
	if _, err := build(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "a: token_value_file") {
		t.Errorf("missing file: got %v, want an error naming the module", err)
	}
}

// A config file path with a line break is rejected before it can reach a log
// line. The check runs before the file is read, so no file is needed.
func TestLoadConfigRejectsControlCharsInPath(t *testing.T) {
	for _, path := range []string{"pbs\nINFO: forged.yaml", "pbs\r.yaml"} {
		_, _, err := LoadConfig(filepath.Join(t.TempDir(), path))
		if err == nil || err.Error() != "config file path must not contain control characters" {
			t.Errorf("%q: got error %v, want the control character error", path, err)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("modules:\n  site:\n"+validModule), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, modules, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules["site"] == nil || cfg.Modules["site"].Endpoint != "https://pbs:8007" {
		t.Errorf("modules = %v", moduleNames(modules))
	}
	if _, _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Error("expected an error for a missing config file")
	}
}

func TestValidateBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://pbs:8007":   "https://pbs:8007",
		"https://pbs:8007/":  "https://pbs:8007",
		"http://10.0.0.1":    "http://10.0.0.1",
		"https://[::1]:8007": "https://[::1]:8007",
	} {
		if got, err := validateBaseURL(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "pbs:8007", "https://", "https://:8007", "file:///etc/passwd", "https://pbs:8007/x", "https://pbs:8007?a", "https://pbs:8007#", "https://u@pbs:8007", "https://pbs\n:8007"} {
		if _, err := validateBaseURL(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

// The environment variables of earlier versions are reported, not used.
func TestLegacyEnvSet(t *testing.T) {
	for _, name := range removedEnv {
		// restored after the test by Setenv
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	if got := legacyEnvSet(); len(got) != 0 {
		t.Errorf("got %v without environment variables", got)
	}
	t.Setenv("PBS_ENDPOINT", "https://pbs:8007")
	t.Setenv("PBS_API_TOKEN_FILE", "/run/secrets/x")
	if got := legacyEnvSet(); !slices.Equal(got, []string{"PBS_ENDPOINT", "PBS_API_TOKEN_FILE"}) {
		t.Errorf("got %v", got)
	}
}

// The example config in the repository is valid.
func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile("pbs-exporter.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Modules) != 2 || cfg.Modules["default"].TokenValueFile == "" || !cfg.Modules["site-b"].Collectors["access"] {
		t.Errorf("unexpected example config: %+v", cfg.Modules)
	}
}
