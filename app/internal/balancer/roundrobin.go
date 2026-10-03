package balancer

import (
	"sync/atomic"
	"time"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

// preLen is how many ordered candidates are precomputed per schedule slot so
// that Pick(n<=preLen) allocates nothing. The retry budget is 3.
const preLen = 3

// roundRobin is smooth weighted round-robin (nginx style). Rebuild expands the
// effective weights into a schedule (length = sum of gcd-reduced weights) and
// stores it in an atomic.Pointer; Pick is one atomic counter increment plus a
// table lookup.
//
// Slow-start is approximate: the schedule is computed from the weights at build
// time. While any backend is still ramping, Pick re-derives the schedule at
// most every slowStart/20 (min 50ms), one goroutine at a time, so the ramp
// advances in steps instead of waiting for the next snapshot change.
type roundRobin struct {
	opts       Options
	counter    atomic.Uint64
	state      atomic.Pointer[rrState]
	rebuilding atomic.Bool
}

type rrState struct {
	id        ident
	healthy   []*backend.Backend
	sched     []int32
	lists     [][]*backend.Backend // lists[i]: i and its next preLen-1 cyclic successors
	builtAt   int64                // unix nanos
	rampUntil int64                // unix nanos; 0 = no backend ramping
}

func newRoundRobin(opts Options) *roundRobin { return &roundRobin{opts: opts} }

func (r *roundRobin) Rebuild(snap *backend.Snapshot) {
	r.state.Store(r.build(snap.Healthy))
}

func (r *roundRobin) build(healthy []*backend.Backend) *rrState {
	now := r.opts.Now()
	st := &rrState{id: identOf(healthy), healthy: healthy, builtAt: now.UnixNano()}
	n := len(healthy)
	if n == 0 {
		return st
	}
	w := make([]int, n)
	g := 0
	for i, b := range healthy {
		w[i] = EffectiveWeight(b, r.opts.SlowStart, now)
		g = gcd(g, w[i])
		if r.opts.SlowStart > 0 {
			if end := b.HealthySince().Add(r.opts.SlowStart).UnixNano(); end > now.UnixNano() && end > st.rampUntil {
				st.rampUntil = end
			}
		}
	}
	total := 0
	for i := range w {
		w[i] /= g
		total += w[i]
	}
	cur := make([]int, n)
	st.sched = make([]int32, 0, total)
	for s := 0; s < total; s++ {
		best := 0
		for i := range cur {
			cur[i] += w[i]
			if cur[i] > cur[best] {
				best = i
			}
		}
		cur[best] -= total
		st.sched = append(st.sched, int32(best))
	}
	k := min(n, preLen)
	st.lists = make([][]*backend.Backend, n)
	for i := range st.lists {
		l := make([]*backend.Backend, k)
		for j := 0; j < k; j++ {
			l[j] = healthy[(i+j)%n]
		}
		st.lists[i] = l
	}
	return st
}

func (r *roundRobin) get(healthy []*backend.Backend) *rrState {
	st := r.state.Load()
	if st == nil || len(st.sched) == 0 {
		// Pick before any Rebuild (or after one over an empty set): nothing
		// to be stale against, build from this view. Never rebuild otherwise
		// (see Pick): a Pick holding an older view must not overwrite newer
		// state.
		st = r.build(healthy)
		r.state.Store(st)
		return st
	}
	if st.rampUntil != 0 {
		step := r.opts.SlowStart / 20
		if step < 50*time.Millisecond {
			step = 50 * time.Millisecond
		}
		if r.opts.Now().UnixNano()-st.builtAt >= int64(step) && r.rebuilding.CompareAndSwap(false, true) {
			st = r.build(st.healthy) // same membership; only the ramped weights move
			r.state.Store(st)
			r.rebuilding.Store(false)
		}
	}
	return st
}

// Pick uses the schedule from the latest Rebuild even if snap is a view older
// or newer than it: for the microseconds between a publish's Rebuild and its
// view store (or the reverse for a stale reader) it may return a backend that
// was just ejected or just removed from the pool. That is acceptable: an
// ejected backend fails its dial and is retried elsewhere, and a removed
// Backend is still a valid object whose dial fails fast.
func (r *roundRobin) Pick(snap *backend.Snapshot, _ Key, n int) []*backend.Backend {
	if n <= 0 || len(snap.Healthy) == 0 {
		return nil
	}
	st := r.get(snap.Healthy)
	start := int(st.sched[(r.counter.Add(1)-1)%uint64(len(st.sched))])
	if l := st.lists[start]; n <= len(l) || len(l) == len(st.healthy) {
		l = l[:min(n, len(l))]
		ok := true
		for _, b := range l {
			if full(b) {
				ok = false
				break
			}
		}
		if ok {
			return l // shared, read-only, zero allocation
		}
	}
	// Slow path: walk cyclically from the scheduled backend, skipping over-cap.
	out := make([]*backend.Backend, 0, min(n, len(st.healthy)))
	for j := 0; j < len(st.healthy) && len(out) < n; j++ {
		if b := st.healthy[(start+j)%len(st.healthy)]; !full(b) {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
