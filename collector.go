package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

const promNamespace = "pbs"

// collector collects one group of metrics. Collectors run concurrently; one
// that fails does not stop the others.
type collector struct {
	name string
	// enabled is the default, a module can override it.
	enabled bool
	// required collectors can not be disabled.
	required bool
	// privilege is named in the log message when PBS answers 403.
	privilege string
	descs     []*prometheus.Desc
	// collect sends its metrics to ch. Errors of single items (a namespace, a
	// datastore) do not stop it; it returns them joined after the other items
	// were collected.
	collect func(ctx context.Context, s *scrapeState, ch chan<- prometheus.Metric) error
}

// allCollectors returns every collector, in the order they are reported.
func allCollectors() []*collector {
	return []*collector{
		versionCollector,
		nodeCollector,
		datastoresCollector,
		groupsCollector,
		jobsCollector,
		syncCollector,
		tasksCollector,
		zfsCollector,
		timeCollector,
		aptCollector,
		accessCollector,
		disksCollector,
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
		case findCollector(name).required && !on:
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
		if on || c.required {
			enabled = append(enabled, c)
		}
	}
	return enabled
}

// shared fetches a value once and shares it between the collectors of a
// scrape. A panic in fetch is returned as error to every caller.
type shared[V any] struct {
	once sync.Once
	v    V
	err  error
}

func (s *shared[V]) get(fetch func() (V, error)) (V, error) {
	s.once.Do(func() {
		s.err = recoverErr(func() (err error) {
			s.v, err = fetch()
			return err
		})
	})
	return s.v, s.err
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
	api *apiClient
	// maxConcurrency limits the concurrent items (datastores, namespaces) a
	// collector walks; the requests themselves are limited by api.
	maxConcurrency int

	datastoreUsage shared[[]datastoreUsage]
	datastoreList  shared[[]datastoreListItem]
	permissions    shared[permissions]
}

// newDesc returns the descriptor of the metric pbs_<subsystem>_<name>
// (pbs_<name> without subsystem).
func newDesc(subsystem, name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(promNamespace, subsystem, name), help, labels, nil)
}

// gauge sends a gauge sample.
func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

// optGauge sends a gauge sample if v is set.
func optGauge[T int64 | float64](ch chan<- prometheus.Metric, d *prometheus.Desc, v *T, labels ...string) {
	if v != nil {
		gauge(ch, d, float64(*v), labels...)
	}
}

var (
	pbsUp = newDesc("", "up",
		"Was the last query of PBS successful (the version call).")
	// the module is the target label Prometheus sets (see the README)
	pbsInfo = newDesc("", "info",
		"Endpoint host of the scraped PBS, always 1.",
		"endpoint_host")
	collectorSuccess = newDesc("collector", "success",
		"Whether a collector succeeded in the last scrape.",
		"collector")
	collectorDuration = newDesc("collector", "duration_seconds",
		"Duration of a collector in the last scrape.",
		"collector")
)

type Exporter struct {
	ctx     context.Context
	timeout time.Duration
	module  *Module
}

// NewExporter returns the collector of one scrape of the module's endpoint.
// All PBS requests stop when ctx is done or after timeout, whichever is first.
func NewExporter(ctx context.Context, module *Module, timeout time.Duration) *Exporter {
	return &Exporter{ctx: ctx, timeout: timeout, module: module}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- pbsUp
	ch <- pbsInfo
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

	s := &scrapeState{api: e.module.api, maxConcurrency: e.module.maxConcurrency}
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
		} else if c == versionCollector {
			// pbs_up is derived from the version call
			upValue = 1
		}
		gauge(ch, collectorSuccess, success, c.name)
		gauge(ch, collectorDuration, durations[i].Seconds(), c.name)
	}
	gauge(ch, pbsUp, upValue)
	gauge(ch, pbsInfo, 1, e.module.host)
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
