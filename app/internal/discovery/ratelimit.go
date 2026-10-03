package discovery

import (
	"sync"
	"time"
)

// rateLimiter lets one event per key through per interval.
type rateLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	last     map[string]time.Time
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{interval: interval, last: make(map[string]time.Time)}
}

// allow reports whether key may fire at now, and if so starts its interval.
func (r *rateLimiter) allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.last[key]; ok && now.Sub(t) < r.interval {
		return false
	}
	r.last[key] = now
	return true
}
