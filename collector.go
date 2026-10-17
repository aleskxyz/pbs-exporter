package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const promNamespace = "pbs"

// collector collects one group of metrics. Collectors run concurrently; one
// that fails does not stop the others.
type collector struct {
	name string
	// enabled is the default, a module can override it (except for "version").
	enabled bool
	// privilege is named in the log message when PBS answers 403.
	privilege string
	descs     []*prometheus.Desc
	// collect sends its metrics to ch. Errors of single items (a namespace, a
	// datastore) do not stop it; it returns them joined after the other items
	// were collected.
	collect func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error
}

// versionCollectorName is the collector pbs_up is derived from; it can not be
// disabled.
const versionCollectorName = "version"

// allCollectors returns every collector, in the order they are reported.
func allCollectors() []*collector {
	return []*collector{
		versionCollector,
		nodeCollector,
		datastoresCollector,
		snapshotsCollector,
		groupsCollector,
		jobsCollector,
		syncCollector,
		tasksCollector,
	}
}

func findCollector(name string) *collector {
	for _, c := range allCollectors() {
		if c.name == name {
			return c
		}
	}
	return nil
}

func collectorNames() []string {
	var names []string
	for _, c := range allCollectors() {
		names = append(names, c.name)
	}
	sort.Strings(names)
	return names
}

// validateCollectors checks the collector overrides of a module.
func validateCollectors(overrides map[string]bool) error {
	var invalid []string
	for name, on := range overrides {
		switch {
		case findCollector(name) == nil:
			invalid = append(invalid, fmt.Sprintf("unknown collector %q (known: %s)", name, strings.Join(collectorNames(), ", ")))
		case name == versionCollectorName && !on:
			invalid = append(invalid, fmt.Sprintf("collector %q can not be disabled", name))
		}
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		return errors.New(strings.Join(invalid, "; "))
	}
	return nil
}

// enabledCollectors applies the overrides of a module to the defaults.
func enabledCollectors(overrides map[string]bool) []*collector {
	var enabled []*collector
	for _, c := range allCollectors() {
		on := c.enabled
		if v, ok := overrides[c.name]; ok {
			on = v
		}
		if on || c.name == versionCollectorName {
			enabled = append(enabled, c)
		}
	}
	return enabled
}

// parseCollectorList parses a comma separated list of collectors to enable
// ("name") or disable ("-name"), e.g. "access,-apt".
func parseCollectorList(s string) (map[string]bool, error) {
	overrides := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		on := true
		if name, ok := strings.CutPrefix(item, "-"); ok {
			item, on = name, false
		}
		overrides[item] = on
	}
	if err := validateCollectors(overrides); err != nil {
		return nil, err
	}
	return overrides, nil
}

// memo fetches a value once per key and shares it between the collectors of a
// scrape.
type memo[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]func() (V, error)
}

func (m *memo[K, V]) get(key K, fetch func() (V, error)) (V, error) {
	m.mu.Lock()
	f, ok := m.m[key]
	if !ok {
		if m.m == nil {
			m.m = map[K]func() (V, error){}
		}
		f = sync.OnceValues(fetch)
		m.m[key] = f
	}
	m.mu.Unlock()
	return f()
}

// forEach calls f for every item, at most limit at a time, and returns the
// errors of all calls joined. A panic in f is returned as an error.
func forEach[T any](items []T, limit int, f func(T) error) error {
	var g errgroup.Group
	g.SetLimit(limit)
	var mu sync.Mutex
	var errs []error
	for _, item := range items {
		g.Go(func() error {
			if err := recoverErr(func() error { return f(item) }); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return errors.Join(errs...)
}

// scrapeState holds the state of one scrape: the API client and the responses
// that more than one collector uses, so that each is fetched only once.
type scrapeState struct {
	api    *apiClient
	module *Module
	// now is when the scrape started
	now time.Time

	datastoreUsage memo[struct{}, []datastoreUsage]
	datastoreList  memo[struct{}, []datastoreListItem]
	namespaces     memo[string, []namespaceItem]
	snapshots      memo[storeNamespace, []snapshotItem]
	tasks          memo[struct{}, []taskItem]
	permissions    memo[struct{}, permissions]
}

type storeNamespace struct {
	store, ns string
}

var (
	up = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "up"),
		"Was the last query of PBS successful (the version call).",
		nil, nil,
	)
	info = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "", "info"),
		"Module and endpoint host of the scraped PBS.",
		[]string{"module", "endpoint_host"}, nil,
	)
	collectorSuccess = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "collector", "success"),
		"Whether a collector succeeded in the last scrape.",
		[]string{"collector"}, nil,
	)
	collectorDuration = prometheus.NewDesc(
		prometheus.BuildFQName(promNamespace, "collector", "duration_seconds"),
		"Duration of a collector in the last scrape.",
		[]string{"collector"}, nil,
	)
)

type Exporter struct {
	ctx     context.Context
	timeout time.Duration
	module  *Module
	api     *apiClient
}

// NewExporter returns the collector of one scrape of endpoint with module.
// All PBS requests stop when ctx is done or after timeout, whichever is first.
func NewExporter(ctx context.Context, endpoint string, module *Module, timeout time.Duration) *Exporter {
	sem := module.sem
	if module.endpoint == "" {
		// the legacy module scrapes any "target": a limit shared by all
		// targets would let one slow PBS block the scrapes of the others
		sem = semaphore.NewWeighted(int64(module.maxConcurrency))
	}
	return &Exporter{
		ctx:     ctx,
		timeout: timeout,
		module:  module,
		api: &apiClient{
			endpoint:            endpoint,
			authorizationHeader: module.authorizationHeader,
			client:              module.client,
			sem:                 sem,
		},
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- up
	ch <- info
	ch <- collectorSuccess
	ch <- collectorDuration
	// collectors may share descriptors (e.g. the job metrics of jobs and
	// sync), the registry refuses duplicates
	seen := map[*prometheus.Desc]bool{}
	for _, c := range e.module.collectors {
		for _, d := range c.descs {
			if !seen[d] {
				seen[d] = true
				ch <- d
			}
		}
	}
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(e.ctx, e.timeout)
	defer cancel()

	s := &scrapeState{api: e.api, module: e.module, now: time.Now()}
	collectors := e.module.collectors
	errs := make([]error, len(collectors))
	durations := make([]time.Duration, len(collectors))
	var wg sync.WaitGroup
	for i, c := range collectors {
		wg.Go(func() {
			start := time.Now()
			errs[i] = runCollector(ctx, c, s, ch)
			durations[i] = time.Since(start)
		})
	}
	wg.Wait()

	upValue := 0.0
	for i, c := range collectors {
		success := 1.0
		if err := errs[i]; err != nil {
			success = 0
			attrs := []any{"module", sanitize(e.module.name), "collector", c.name, "err", errText(err)}
			if IsForbidden(err) && c.privilege != "" {
				attrs = append(attrs, "required_privilege", c.privilege)
			}
			slog.Warn("collector failed", attrs...)
		} else if c.name == versionCollectorName {
			upValue = 1
		}
		ch <- prometheus.MustNewConstMetric(collectorSuccess, prometheus.GaugeValue, success, c.name)
		ch <- prometheus.MustNewConstMetric(collectorDuration, prometheus.GaugeValue, durations[i].Seconds(), c.name)
	}
	ch <- prometheus.MustNewConstMetric(up, prometheus.GaugeValue, upValue)
	ch <- prometheus.MustNewConstMetric(info, prometheus.GaugeValue, 1, e.module.name, endpointHost(e.api.endpoint))
}

// runCollector runs c. A panic becomes an error, so a bad PBS answer can not
// stop the exporter.
func runCollector(ctx context.Context, c *collector, s *scrapeState, ch chan<- prometheus.Metric) error {
	return recoverErr(func() error { return c.collect(ctx, s, ch) })
}

// joinUnique joins errors like errors.Join, but each message only once: a
// shared response that failed is reported once, not by every user.
func joinUnique(errs ...error) error {
	seen := map[string]bool{}
	var out []error
	for _, err := range errs {
		if err == nil || seen[err.Error()] {
			continue
		}
		seen[err.Error()] = true
		out = append(out, err)
	}
	return errors.Join(out...)
}

// recoverErr calls f and returns a panic in f as an error.
func recoverErr(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return f()
}

// endpointHost returns the host of endpoint as label value; the "target"
// parameter of the legacy module may contain invalid UTF-8, which a label
// value must not.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(u.Hostname(), "")
}
