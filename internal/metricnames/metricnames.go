// Package metricnames is a Prometheus registry that remembers what was registered, so a
// component can say which metric names it exposes, including those with no sample yet (a vector
// with no children, a collector that emits only when there is data). The dashboards' drift test
// reads it (ADR-0009).
package metricnames

import (
	"regexp"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Registry is a prometheus.Registry that records its collectors.
type Registry struct {
	*prometheus.Registry
	mu         sync.Mutex
	collectors []prometheus.Collector
}

// New returns an empty registry.
func New() *Registry { return &Registry{Registry: prometheus.NewRegistry()} }

// Register registers c and records it.
func (r *Registry) Register(c prometheus.Collector) error {
	if err := r.Registry.Register(c); err != nil {
		return err
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
	return nil
}

// MustRegister registers each collector, panicking on the first failure.
func (r *Registry) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

// Unregister removes c.
func (r *Registry) Unregister(c prometheus.Collector) bool {
	if !r.Registry.Unregister(c) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, have := range r.collectors {
		if have == c {
			r.collectors = append(r.collectors[:i], r.collectors[i+1:]...)
			break
		}
	}
	return true
}

var fqName = regexp.MustCompile(`fqName: "([^"]+)"`)

// Names returns every metric name the registered collectors describe, sorted.
func (r *Registry) Names() []string {
	r.mu.Lock()
	cs := append([]prometheus.Collector(nil), r.collectors...)
	r.mu.Unlock()
	ch := make(chan *prometheus.Desc)
	go func() {
		for _, c := range cs {
			c.Describe(ch)
		}
		close(ch)
	}()
	seen := map[string]bool{}
	for d := range ch {
		if m := fqName.FindStringSubmatch(d.String()); m != nil {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
