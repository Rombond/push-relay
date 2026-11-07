package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/Rombond/push-relay/internal/push"
)

// Metrics holds the relay's counters, exposed in the Prometheus text format.
type Metrics struct {
	mu sync.Mutex
	m  map[[3]string]uint64 // pushes per app, platform, status
	c  map[string]uint64    // other counters, keyed by their full series name
	// Keys returns active keys by kind at scrape time (nil = not exported).
	Keys func() (public, manual int64, err error)
}

func newMetrics() *Metrics { return &Metrics{m: map[[3]string]uint64{}, c: map[string]uint64{}} }

func (m *Metrics) count(app, platform string, st push.Status) {
	m.mu.Lock()
	m.m[[3]string{app, platform, string(st)}]++
	m.mu.Unlock()
}

func (m *Metrics) inc(name, label, value string) {
	m.mu.Lock()
	m.c[fmt.Sprintf("%s{%s=%q}", name, label, value)]++
	m.mu.Unlock()
}

var counters = []string{
	"push_relay_registrations_total",         // new public keys, per app
	"push_relay_rotations_total",             // re-registrations that rotated a key, per app
	"push_relay_registration_refusals_total", // per reason
	"push_relay_quota_refusals_total",        // pushes refused by a daily quota, per app
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		lines := make([]string, 0, len(m.m))
		for k, v := range m.m {
			lines = append(lines, fmt.Sprintf("push_relay_pushes_total{app=%q,platform=%q,status=%q} %d", k[0], k[1], k[2], v))
		}
		other := make([]string, 0, len(m.c))
		for k, v := range m.c {
			other = append(other, fmt.Sprintf("%s %d", k, v))
		}
		m.mu.Unlock()
		sort.Strings(lines)
		sort.Strings(other)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# TYPE push_relay_pushes_total counter")
		fmt.Fprintln(w, strings.Join(lines, "\n"))
		for _, name := range counters {
			fmt.Fprintf(w, "# TYPE %s counter\n", name)
			for _, l := range other {
				if strings.HasPrefix(l, name+"{") {
					fmt.Fprintln(w, l)
				}
			}
		}
		if m.Keys != nil {
			if pub, man, err := m.Keys(); err == nil {
				fmt.Fprintln(w, "# TYPE push_relay_keys gauge")
				fmt.Fprintf(w, "push_relay_keys{kind=\"manual\"} %d\npush_relay_keys{kind=\"public\"} %d\n", man, pub)
			}
		}
	})
}
