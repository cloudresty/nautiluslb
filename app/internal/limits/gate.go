// Package limits provides connection-limiting primitives: a global Gate and a
// per-source-IP PerSource counter. Both are safe for concurrent use.
package limits

import "sync/atomic"

// Gate bounds the number of concurrent holders. A capacity of 0 means
// unlimited (Active is still tracked).
type Gate struct {
	active   atomic.Int64
	capacity atomic.Int64
}

// NewGate returns a Gate with the given capacity (0 = unlimited).
func NewGate(capacity int) *Gate {
	g := &Gate{}
	g.capacity.Store(int64(capacity))
	return g
}

// TryAcquire takes a slot if one is free. It never lets Active exceed a
// non-zero capacity, even under contention.
func (g *Gate) TryAcquire() bool {
	for {
		a := g.active.Load()
		if c := g.capacity.Load(); c > 0 && a >= c {
			return false
		}
		if g.active.CompareAndSwap(a, a+1) {
			return true
		}
	}
}

// Release returns a slot. Underflow is clamped: Active never goes negative.
func (g *Gate) Release() {
	for {
		a := g.active.Load()
		if a <= 0 {
			return
		}
		if g.active.CompareAndSwap(a, a-1) {
			return
		}
	}
}

// Active returns the number of currently held slots.
func (g *Gate) Active() int64 { return g.active.Load() }

// SetCapacity changes the capacity (0 = unlimited). Lowering it below Active
// does not evict holders; it only blocks new acquires until Active drops.
func (g *Gate) SetCapacity(n int) { g.capacity.Store(int64(n)) }
