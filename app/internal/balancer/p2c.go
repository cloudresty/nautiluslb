package balancer

import (
	"math/rand/v2"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

// randomTwoChoices samples two random eligible backends and prefers the one
// with the lower active/effectiveWeight (power of two choices). Stateless.
type randomTwoChoices struct{ opts Options }

func (p *randomTwoChoices) Rebuild(*backend.Snapshot) {}

func (p *randomTwoChoices) Pick(snap *backend.Snapshot, _ Key, n int) []*backend.Backend {
	h := snap.Healthy
	if n <= 0 || len(h) == 0 {
		return nil
	}
	n = min(n, len(h))
	now := p.opts.Now()
	out := make([]*backend.Backend, 0, n)
	ok := func(b *backend.Backend) bool { return !full(b) && !contains(out, b) }
	sc := func(b *backend.Backend) scored {
		return scored{b, b.ActiveConnections(), int64(EffectiveWeight(b, p.opts.SlowStart, now))}
	}
	for try := 0; len(out) < n && try < 8*n; try++ {
		a, b := h[rand.IntN(len(h))], h[rand.IntN(len(h))] //nolint:gosec // G404: load-balancing choice, not a security boundary
		switch {
		case ok(a) && ok(b) && a != b:
			if sc(b).less(sc(a)) {
				a = b
			}
			out = append(out, a)
		case ok(a):
			out = append(out, a)
		case ok(b):
			out = append(out, b)
		}
	}
	// Guarantee: fill from a random start so a lone eligible backend is found.
	if len(out) < n {
		s := rand.IntN(len(h)) //nolint:gosec // G404: load-balancing choice, not a security boundary
		for i := 0; i < len(h) && len(out) < n; i++ {
			if b := h[(s+i)%len(h)]; ok(b) {
				out = append(out, b)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
