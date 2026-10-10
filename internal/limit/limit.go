// Package limit is a keyed token bucket (one bucket per server key or per device token).
package limit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

type Limiter struct {
	rate  rate.Limit
	burst int
	now   func() time.Time

	mu    sync.Mutex
	m     map[string]*entry
	swept time.Time
}

func New(r rate.Limit, burst int) *Limiter {
	return &Limiter{rate: r, burst: burst, now: time.Now, m: map[string]*entry{}}
}

// Allow takes one token. When the bucket is empty it returns false and how long to wait.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	e := l.m[key]
	if e == nil {
		e = &entry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.m[key] = e
	}
	e.seen = now
	r := e.lim.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// sweep drops idle buckets once a minute so the map cannot grow without bound.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	for k, e := range l.m {
		if now.Sub(e.seen) > 10*time.Minute {
			delete(l.m, k)
		}
	}
}
