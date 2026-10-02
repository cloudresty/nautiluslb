// Package balancer selects backends for a pool. Every Picker is lock-free on
// the hot path (atomics and atomic.Pointer only).
package balancer

import (
	"errors"
	"net/netip"
	"time"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

// ErrUnknownAlgorithm is returned by New for an unsupported algorithm name.
var ErrUnknownAlgorithm = errors.New("balancer: unknown algorithm")

// Key carries what a picker may hash on.
type Key struct{ SourceIP netip.Addr }

// Picker chooses backends.
//
// Pick returns up to n distinct candidates in preference order, chosen from
// snap.Healthy. Backends at their MaxConnections cap are skipped. It returns
// nil when nothing is eligible. The result must be treated as read-only (the
// round-robin fast path returns shared immutable slices).
//
// Fail-open lives in the pool, not here: when Pick(snap) returns nothing the
// pool calls it again with a Snapshot whose Healthy is the full set.
//
// Rebuild precomputes per-snapshot state (schedule, hash ring) and is called on
// every snapshot change. It is safe to call concurrently with Pick; Pick also
// rebuilds lazily if it is handed a snapshot Rebuild has not seen, so a missed
// Rebuild costs latency, never correctness.
type Picker interface {
	Pick(snap *backend.Snapshot, key Key, n int) []*backend.Backend
	Rebuild(snap *backend.Snapshot)
}

// Options configures a Picker.
type Options struct {
	// SlowStart ramps a newly healthy backend's weight from 10% to 100% over
	// this duration. Zero disables it.
	SlowStart time.Duration
	// Now overrides the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// New returns a Picker for: round_robin, least_conn, source_ip_hash,
// random_two_choices.
func New(algorithm string, opts Options) (Picker, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	switch algorithm {
	case "round_robin":
		return newRoundRobin(opts), nil
	case "least_conn":
		return &leastConn{opts: opts}, nil
	case "source_ip_hash":
		return &sourceIPHash{}, nil
	case "random_two_choices":
		return &randomTwoChoices{opts: opts}, nil
	}
	return nil, ErrUnknownAlgorithm
}

// EffectiveWeight is the backend's weight after slow-start: it ramps linearly
// from 10% to 100% of Endpoint.Weight over slowStart since HealthySince. The
// result is at least 1.
func EffectiveWeight(b *backend.Backend, slowStart time.Duration, now time.Time) int {
	w := b.Endpoint().Weight
	if w < 1 {
		w = 1
	}
	if slowStart <= 0 {
		return w
	}
	el := now.Sub(b.HealthySince())
	if el >= slowStart {
		return w
	}
	if el < 0 {
		el = 0
	}
	f := 0.1 + 0.9*float64(el)/float64(slowStart)
	ew := int(float64(w)*f + 0.5)
	if ew < 1 {
		ew = 1
	}
	return ew
}

// full reports whether b is at its connection cap.
func full(b *backend.Backend) bool {
	m := b.MaxConnections()
	return m > 0 && b.ActiveConnections() >= int64(m)
}

// ident identifies a backend slice by its first element and length, so a
// precomputed state can be matched to the snapshot slice it was built from.
type ident struct {
	p **backend.Backend
	n int
}

func identOf(s []*backend.Backend) ident {
	if len(s) == 0 {
		return ident{}
	}
	return ident{p: &s[0], n: len(s)}
}

func contains(s []*backend.Backend, b *backend.Backend) bool {
	for _, x := range s {
		if x == b {
			return true
		}
	}
	return false
}
