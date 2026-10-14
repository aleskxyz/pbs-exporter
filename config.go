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
)

const defaultModuleName = "default"

// defaultModuleTimeout is the timeout of a config file module that sets none.
// The config file does not inherit pbs.timeout / PBS_TIMEOUT.
const defaultModuleTimeout = 5 * time.Second

// ModuleConfig holds the credentials and connection settings used to talk to a
// Proxmox Backup Server. A module is selected per scrape with the "module" URL
// parameter, the same way prometheus-pve-exporter does it.
type ModuleConfig struct {
	// User is the PBS user the API token belongs to, e.g. "root@pam".
	User string `yaml:"user"`
	// TokenName is the name of the API token.
	TokenName string `yaml:"token_name"`
	// TokenValue is the secret of the API token.
	TokenValue string `yaml:"token_value"`
	// Endpoint is the PBS the module scrapes (required). The "target" URL
	// parameter is ignored (like PBS_ENDPOINT) and redirects are not followed,
	// so callers of the exporter can not send the token to any other server.
	Endpoint string `yaml:"endpoint"`
	// Insecure disables TLS certificate verification.
	Insecure bool `yaml:"insecure"`
	// Timeout of requests to PBS (Go duration), defaults to defaultModuleTimeout.
	Timeout string `yaml:"timeout"`
}

// Module is a validated ModuleConfig with a dedicated HTTP client, so that
// TLS and timeout settings are not shared between modules.
type Module struct {
	name                string
	endpoint            string
	authorizationHeader string
	client              *http.Client
}

// Config is the content of the config file: module name -> module settings.
type Config map[string]ModuleConfig

// LoadConfig reads and parses a YAML config file.
func LoadConfig(path string) (Config, error) {
	// the path is logged, also as part of the read error
	if hasControlChar(path) {
		return nil, errors.New("config file path must not contain control characters")
	}
	// #nosec G304 G703 -- path is set by the operator via flag/env, not untrusted input
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("unable to read config file: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses the YAML content of a config file.
func ParseConfig(data []byte) (Config, error) {
	cfg := Config{}
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
	if len(cfg) == 0 {
		return nil, fmt.Errorf("invalid config file: no modules specified")
	}
	var invalid []string
	for name, m := range cfg {
		if name == "" {
			invalid = append(invalid, "  - module name must not be empty")
			continue
		}
		// module names are logged; the name itself is not part of the message
		if hasControlChar(name) {
			invalid = append(invalid, "  - module name must not contain control characters")
			continue
		}
		if m.User == "" || m.TokenName == "" || m.TokenValue == "" {
			invalid = append(invalid, fmt.Sprintf("  - %s: user, token_name and token_value are required", name))
		} else if hasControlChar(m.User) || hasControlChar(m.TokenName) || hasControlChar(m.TokenValue) {
			invalid = append(invalid, fmt.Sprintf("  - %s: user, token_name and token_value must not contain control characters", name))
		}
		if m.Endpoint == "" {
			invalid = append(invalid, fmt.Sprintf("  - %s: endpoint is required", name))
		} else {
			endpoint, err := validateBaseURL(m.Endpoint)
			if err != nil {
				invalid = append(invalid, fmt.Sprintf("  - %s: endpoint %s", name, err))
			}
			m.Endpoint = endpoint
		}
		if m.Timeout != "" {
			if d, err := time.ParseDuration(m.Timeout); err != nil {
				invalid = append(invalid, fmt.Sprintf("  - %s: unable to parse timeout: %s", name, err))
			} else if d <= 0 {
				invalid = append(invalid, fmt.Sprintf("  - %s: timeout must be greater than 0", name))
			}
		}
		cfg[name] = m
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return nil, fmt.Errorf("invalid module config entries in config file:\n%s", strings.Join(invalid, "\n"))
	}
	return cfg, nil
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

// hasNullModuleName reports whether the top-level mapping has a null key.
func hasNullModuleName(data []byte) bool {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) == 0 {
		return false
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Tag == "!!null" {
			return true
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

// NewModule builds a Module. defaultTimeout is used when the module sets no timeout.
func NewModule(name string, cfg ModuleConfig, defaultTimeout time.Duration) (*Module, error) {
	timeout := defaultTimeout
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("module %q: unable to parse timeout: %w", name, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("module %q: timeout must be greater than 0", name)
		}
		timeout = d
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// #nosec G402 -- opt-in per module, mirrors the existing PBS_INSECURE option
			InsecureSkipVerify: cfg.Insecure,
		},
	}
	return &Module{
		name:                name,
		endpoint:            cfg.Endpoint,
		authorizationHeader: "PBSAPIToken=" + cfg.User + "!" + cfg.TokenName + ":" + cfg.TokenValue,
		client:              &http.Client{Transport: tr, Timeout: timeout},
	}, nil
}

// BuildModules creates the modules of a config file (cfg) or, without a config
// file, the legacy "default" module built from the flags/environment variables
// (fallback). The two are not mixed: with a config file, fallback is nil.
func BuildModules(cfg Config, fallback *ModuleConfig, defaultTimeout time.Duration) (map[string]*Module, error) {
	modules := map[string]*Module{}
	for name, mc := range cfg {
		// without a pinned endpoint the "target" URL parameter would decide
		// where the module's token is sent
		if mc.Endpoint == "" {
			return nil, fmt.Errorf("module %q: endpoint is required", name)
		}
		m, err := NewModule(name, mc, defaultTimeout)
		if err != nil {
			return nil, err
		}
		// do not follow redirects, so the token is never sent to another host
		m.client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		modules[name] = m
	}
	if _, ok := modules[defaultModuleName]; !ok && fallback != nil {
		m, err := NewModule(defaultModuleName, *fallback, defaultTimeout)
		if err != nil {
			return nil, err
		}
		modules[defaultModuleName] = m
	}
	return modules, nil
}

// LoadModules loads the config file and builds its modules. Flags and
// environment variables for the connection (pbs.endpoint, pbs.username,
// pbs.api.token, pbs.api.token.name, pbs.insecure, pbs.timeout) are not used.
func LoadModules(path string) (Config, map[string]*Module, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	modules, err := BuildModules(cfg, nil, defaultModuleTimeout)
	if err != nil {
		return nil, nil, err
	}
	return cfg, modules, nil
}
