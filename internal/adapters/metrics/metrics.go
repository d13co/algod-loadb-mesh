// Package metrics is a dependency-free Prometheus text exposition of counters,
// gauges and simple summaries.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry implements ports.Metrics.
type Registry struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64
	sums     map[string]float64
	counts   map[string]float64
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{counters: map[string]float64{}, gauges: map[string]float64{}, sums: map[string]float64{}, counts: map[string]float64{}}
}

func key(name string, labels []string) string {
	if len(labels) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", labels[i], labels[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func (r *Registry) Inc(name string, labels ...string) {
	r.mu.Lock()
	r.counters[key(name, labels)]++
	r.mu.Unlock()
}

func (r *Registry) Observe(name string, v float64, labels ...string) {
	r.mu.Lock()
	k := key(name, labels)
	r.sums[k] += v
	r.counts[k]++
	r.mu.Unlock()
}

func (r *Registry) Gauge(name string, v float64, labels ...string) {
	r.mu.Lock()
	r.gauges[key(name, labels)] = v
	r.mu.Unlock()
}

// WriteTo renders the Prometheus text format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	dump := func(m map[string]float64, suffix string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			name, labels, _ := strings.Cut(k, "{")
			if labels != "" {
				labels = "{" + labels
			}
			fmt.Fprintf(&b, "%s%s%s %g\n", name, suffix, labels, m[k])
		}
	}
	dump(r.counters, "_total")
	dump(r.gauges, "")
	dump(r.sums, "_sum")
	dump(r.counts, "_count")
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}
