package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/Rombond/push-relay/internal/push"
)

// Metrics counts pushes per app, platform and status, exposed in the Prometheus text format.
type Metrics struct {
	mu sync.Mutex
	m  map[[3]string]uint64
}

func newMetrics() *Metrics { return &Metrics{m: map[[3]string]uint64{}} }

func (m *Metrics) count(app, platform string, st push.Status) {
	m.mu.Lock()
	m.m[[3]string{app, platform, string(st)}]++
	m.mu.Unlock()
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		lines := make([]string, 0, len(m.m))
		for k, v := range m.m {
			lines = append(lines, fmt.Sprintf("push_relay_pushes_total{app=%q,platform=%q,status=%q} %d", k[0], k[1], k[2], v))
		}
		m.mu.Unlock()
		sort.Strings(lines)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# TYPE push_relay_pushes_total counter")
		fmt.Fprintln(w, strings.Join(lines, "\n"))
	})
}
