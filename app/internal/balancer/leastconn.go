package balancer

import (
	"sync/atomic"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

// leastConn prefers the backend with the lowest active/effectiveWeight. Ties
// are broken by a rotating start index so equal backends share load. Score
// comparison uses cross-multiplication (no floats). Stateless: Rebuild is a no-op.
type leastConn struct {
	opts Options
	rot  atomic.Uint64
}

func (l *leastConn) Rebuild(*backend.Snapshot) {}

type scored struct {
	b      *backend.Backend
	active int64
	w      int64
}

// less: a.active/a.w < b.active/b.w
func (a scored) less(b scored) bool { return a.active*b.w < b.active*a.w }

func (l *leastConn) Pick(snap *backend.Snapshot, _ Key, n int) []*backend.Backend {
	h := snap.Healthy
	if n <= 0 || len(h) == 0 {
		return nil
	}
	now := l.opts.Now()
	var arr [4]scored
	top := arr[:0]
	if n > len(arr) {
		top = make([]scored, 0, n)
	}
	start := int(l.rot.Add(1) % uint64(len(h))) //nolint:gosec // G115: the remainder is < len(h), so it fits an int
	for i := range h {
		b := h[(start+i)%len(h)]
		if full(b) {
			continue
		}
		c := scored{b, b.ActiveConnections(), int64(EffectiveWeight(b, l.opts.SlowStart, now))}
		if len(top) == n && !c.less(top[len(top)-1]) {
			continue
		}
		if len(top) < n {
			top = append(top, c)
		} else {
			top[len(top)-1] = c
		}
		// insertion: keep sorted, stable (earlier-found wins ties)
		for j := len(top) - 1; j > 0 && top[j].less(top[j-1]); j-- {
			top[j], top[j-1] = top[j-1], top[j]
		}
	}
	if len(top) == 0 {
		return nil
	}
	out := make([]*backend.Backend, len(top))
	for i := range top {
		out[i] = top[i].b
	}
	return out
}
