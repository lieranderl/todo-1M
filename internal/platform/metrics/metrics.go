package metrics

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Opts struct {
	Name string
	Help string
}

type collector interface {
	name() string
	writePrometheus(*strings.Builder)
}

type Registry struct {
	mu         sync.RWMutex
	collectors map[string]collector
}

func NewRegistry() *Registry {
	return &Registry{
		collectors: map[string]collector{},
	}
}

func (r *Registry) MustRegister(items ...collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range items {
		name := item.name()
		if _, exists := r.collectors[name]; exists {
			panic("metrics collector already registered: " + name)
		}
		r.collectors[name] = item
	}
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var sb strings.Builder

		r.mu.RLock()
		names := make([]string, 0, len(r.collectors))
		for name := range r.collectors {
			names = append(names, name)
		}
		sort.Strings(names)
		collectors := make([]collector, 0, len(names))
		for _, name := range names {
			collectors = append(collectors, r.collectors[name])
		}
		r.mu.RUnlock()

		for _, c := range collectors {
			c.writePrometheus(&sb)
		}
		_, _ = w.Write([]byte(sb.String()))
	})
}

var Default = NewRegistry()
var processStart = time.Now()

func DefaultHandler() http.Handler {
	return Default.Handler()
}

type Gauge struct {
	opts  Opts
	mu    sync.RWMutex
	value float64
}

func NewGauge(opts Opts) *Gauge {
	return &Gauge{opts: opts}
}

func (g *Gauge) name() string {
	return g.opts.Name
}

func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.value = v
	g.mu.Unlock()
}

func (g *Gauge) Add(v float64) {
	g.mu.Lock()
	g.value += v
	g.mu.Unlock()
}

func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

func (g *Gauge) writePrometheus(sb *strings.Builder) {
	g.mu.RLock()
	v := g.value
	g.mu.RUnlock()
	writeMetricHead(sb, g.opts.Name, "gauge", g.opts.Help)
	fmt.Fprintf(sb, "%s %s\n", g.opts.Name, floatToString(v))
}

type GaugeFunc struct {
	opts Opts
	fn   func() float64
}

func NewGaugeFunc(opts Opts, fn func() float64) *GaugeFunc {
	return &GaugeFunc{opts: opts, fn: fn}
}

func (g *GaugeFunc) name() string {
	return g.opts.Name
}

func (g *GaugeFunc) writePrometheus(sb *strings.Builder) {
	writeMetricHead(sb, g.opts.Name, "gauge", g.opts.Help)
	v := 0.0
	if g.fn != nil {
		v = g.fn()
	}
	fmt.Fprintf(sb, "%s %s\n", g.opts.Name, floatToString(v))
}

type CounterVec struct {
	opts       Opts
	labelNames []string

	mu     sync.RWMutex
	values map[string]float64
}

func NewCounterVec(opts Opts, labelNames []string) *CounterVec {
	copied := make([]string, len(labelNames))
	copy(copied, labelNames)
	return &CounterVec{
		opts:       opts,
		labelNames: copied,
		values:     map[string]float64{},
	}
}

func (c *CounterVec) name() string {
	return c.opts.Name
}

func (c *CounterVec) WithLabelValues(values ...string) *Counter {
	return &Counter{parent: c, labelValues: values}
}

func (c *CounterVec) add(labelValues []string, delta float64) {
	if len(labelValues) != len(c.labelNames) {
		return
	}
	key := strings.Join(labelValues, "\xff")
	c.mu.Lock()
	c.values[key] += delta
	c.mu.Unlock()
}

func (c *CounterVec) writePrometheus(sb *strings.Builder) {
	writeMetricHead(sb, c.opts.Name, "counter", c.opts.Help)

	c.mu.RLock()
	entries := make([]struct {
		key   string
		value float64
	}, 0, len(c.values))
	for key, value := range c.values {
		entries = append(entries, struct {
			key   string
			value float64
		}{key: key, value: value})
	}
	c.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].key < entries[j].key
	})

	for _, entry := range entries {
		labelValues := strings.Split(entry.key, "\xff")
		sb.WriteString(c.opts.Name)
		if len(labelValues) > 0 {
			sb.WriteString("{")
			for idx, labelName := range c.labelNames {
				if idx > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(labelName)
				sb.WriteString(`="`)
				sb.WriteString(escapeLabelValue(labelValues[idx]))
				sb.WriteString(`"`)
			}
			sb.WriteString("}")
		}
		sb.WriteString(" ")
		sb.WriteString(floatToString(entry.value))
		sb.WriteString("\n")
	}
}

type Counter struct {
	parent      *CounterVec
	labelValues []string
}

func (c *Counter) Add(v float64) {
	if c == nil || c.parent == nil || v < 0 {
		return
	}
	c.parent.add(c.labelValues, v)
}

func (c *Counter) Inc() { c.Add(1) }

func writeMetricHead(sb *strings.Builder, name, metricType, help string) {
	fmt.Fprintf(sb, "# HELP %s %s\n", name, help)
	fmt.Fprintf(sb, "# TYPE %s %s\n", name, metricType)
}

func floatToString(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

func init() {
	Default.MustRegister(
		NewGaugeFunc(Opts{
			Name: "process_uptime_seconds",
			Help: "Seconds since process start.",
		}, func() float64 {
			return time.Since(processStart).Seconds()
		}),
		NewGaugeFunc(Opts{
			Name: "go_goroutines",
			Help: "Number of goroutines.",
		}, func() float64 {
			return float64(runtime.NumGoroutine())
		}),
		NewGaugeFunc(Opts{
			Name: "go_memstats_alloc_bytes",
			Help: "Allocated heap objects in bytes.",
		}, func() float64 {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			return float64(mem.Alloc)
		}),
		NewGaugeFunc(Opts{
			Name: "go_memstats_heap_inuse_bytes",
			Help: "Heap in-use bytes.",
		}, func() float64 {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			return float64(mem.HeapInuse)
		}),
	)
}
