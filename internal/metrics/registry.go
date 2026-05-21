// Package metrics provides a zero-dependency Prometheus exposition-format
// instrumentation registry for ephemera's control plane.
//
// The registry exposes counters, gauges (eager + functional), and histograms.
// It implements the Prometheus text exposition format 0.0.4 directly, so the
// project keeps its zero-runtime-dependency policy.
//
// Collector types are safe for concurrent use. All numeric mutations use
// sync/atomic (compatible with Go 1.18). Histograms guard their slice updates
// with a small mutex because float64 atomic addition is not natively supported.
package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// namePattern is the validated Prometheus metric/label name format.
// Per the exposition spec, names must match [a-zA-Z_:][a-zA-Z0-9_:]*.
var namePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// validateName panics if the supplied name is not a legal Prometheus name.
// Panic (instead of returning error) is deliberate: registration happens once
// at startup, so an invalid name is a programmer bug and surfaces immediately.
func validateName(name string) {
	if !namePattern.MatchString(name) {
		panic(fmt.Sprintf("metrics: invalid name %q (must match %s)", name, namePattern.String()))
	}
}

// DefaultBuckets is the histogram default when none is supplied. Covers the
// typical control-plane durations from <100ms (probe) to >30s (cold-boot spawn).
var DefaultBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

// Registry holds every collector exposed at /metrics.
type Registry struct {
	mu          sync.RWMutex
	counters    []*Counter
	counterVecs []*CounterVec
	gauges      []*Gauge
	gaugeFuncs  []*GaugeFunc
	histograms  []*Histogram
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Counter is a monotonically increasing 64-bit counter.
type Counter struct {
	name string
	help string
	val  uint64
}

// NewCounter registers and returns a counter. Panics on duplicate name within
// the registry. Counters and counter vecs share the same name space.
func (r *Registry) NewCounter(name, help string) *Counter {
	validateName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assertUniqueLocked(name)
	c := &Counter{name: name, help: help}
	r.counters = append(r.counters, c)
	return c
}

// Inc adds 1 to the counter.
func (c *Counter) Inc() { atomic.AddUint64(&c.val, 1) }

// Add adds n to the counter.
func (c *Counter) Add(n uint64) { atomic.AddUint64(&c.val, n) }

// Get returns the current counter value.
func (c *Counter) Get() uint64 { return atomic.LoadUint64(&c.val) }

// CounterVec is a counter family parameterized by label values.
type CounterVec struct {
	name   string
	help   string
	labels []string

	mu     sync.RWMutex
	values map[string]*Counter
}

// NewCounterVec registers a counter family with the given label keys.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	validateName(name)
	for _, l := range labels {
		validateName(l)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assertUniqueLocked(name)
	v := &CounterVec{
		name:   name,
		help:   help,
		labels: labels,
		values: make(map[string]*Counter),
	}
	r.counterVecs = append(r.counterVecs, v)
	return v
}

// WithLabelValues returns the counter for the given label slot, allocating it
// on first reference. Panics if the value count does not match the label count.
func (v *CounterVec) WithLabelValues(vals ...string) *Counter {
	if len(vals) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s expected %d label values, got %d", v.name, len(v.labels), len(vals)))
	}
	key := strings.Join(vals, "\x00")
	v.mu.RLock()
	c, ok := v.values[key]
	v.mu.RUnlock()
	if ok {
		return c
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok = v.values[key]; ok {
		return c
	}
	c = &Counter{name: v.name, help: v.help}
	v.values[key] = c
	return c
}

// Gauge is a 64-bit signed instantaneous value (can go up or down).
type Gauge struct {
	name string
	help string
	val  int64
}

// NewGauge registers and returns a gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	validateName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assertUniqueLocked(name)
	g := &Gauge{name: name, help: help}
	r.gauges = append(r.gauges, g)
	return g
}

// Set sets the gauge value.
func (g *Gauge) Set(v int64) { atomic.StoreInt64(&g.val, v) }

// Add adds n (may be negative) to the gauge.
func (g *Gauge) Add(n int64) { atomic.AddInt64(&g.val, n) }

// Inc adds 1 to the gauge.
func (g *Gauge) Inc() { atomic.AddInt64(&g.val, 1) }

// Dec subtracts 1 from the gauge.
func (g *Gauge) Dec() { atomic.AddInt64(&g.val, -1) }

// Get returns the current gauge value.
func (g *Gauge) Get() int64 { return atomic.LoadInt64(&g.val) }

// GaugeFunc is a gauge whose value is computed on each exposition by calling
// the supplied function. Used when the value is naturally derived from another
// data structure (e.g. len(map) under a lock).
type GaugeFunc struct {
	name string
	help string
	fn   func() float64
}

// NewGaugeFunc registers a gauge that is re-evaluated on each /metrics scrape.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) *GaugeFunc {
	validateName(name)
	if fn == nil {
		panic("metrics: NewGaugeFunc requires non-nil function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assertUniqueLocked(name)
	g := &GaugeFunc{name: name, help: help, fn: fn}
	r.gaugeFuncs = append(r.gaugeFuncs, g)
	return g
}

// Histogram tracks bucketed observations plus running count + sum.
//
// Buckets store the upper bound of each le bucket; the implicit +Inf bucket
// receives every observation regardless of value. The slice is kept sorted.
type Histogram struct {
	name    string
	help    string
	buckets []float64

	mu     sync.Mutex
	counts []uint64
	sum    float64
	total  uint64
}

// NewHistogram registers a histogram. If buckets is nil or empty, DefaultBuckets
// is used. Buckets are sorted ascending; the +Inf bucket is implicit.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	validateName(name)
	if len(buckets) == 0 {
		buckets = append([]float64(nil), DefaultBuckets...)
	} else {
		buckets = append([]float64(nil), buckets...)
		sort.Float64s(buckets)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assertUniqueLocked(name)
	h := &Histogram{
		name:    name,
		help:    help,
		buckets: buckets,
		counts:  make([]uint64, len(buckets)),
	}
	r.histograms = append(r.histograms, h)
	return h
}

// Observe records a single value, updating bucket counts, total count, and sum.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	h.total++
	h.sum += v
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.mu.Unlock()
}

// snapshot returns immutable copies for exposition.
func (h *Histogram) snapshot() (buckets []float64, counts []uint64, sum float64, total uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bk := make([]float64, len(h.buckets))
	ct := make([]uint64, len(h.counts))
	copy(bk, h.buckets)
	copy(ct, h.counts)
	return bk, ct, h.sum, h.total
}

// assertUniqueLocked panics if name is already registered. Caller holds mu.
func (r *Registry) assertUniqueLocked(name string) {
	for _, c := range r.counters {
		if c.name == name {
			panic(fmt.Sprintf("metrics: duplicate registration %q", name))
		}
	}
	for _, v := range r.counterVecs {
		if v.name == name {
			panic(fmt.Sprintf("metrics: duplicate registration %q", name))
		}
	}
	for _, g := range r.gauges {
		if g.name == name {
			panic(fmt.Sprintf("metrics: duplicate registration %q", name))
		}
	}
	for _, g := range r.gaugeFuncs {
		if g.name == name {
			panic(fmt.Sprintf("metrics: duplicate registration %q", name))
		}
	}
	for _, h := range r.histograms {
		if h.name == name {
			panic(fmt.Sprintf("metrics: duplicate registration %q", name))
		}
	}
}
