// Package backend holds the backend model: an immutable Endpoint produced by
// discovery, a Backend carrying its lock-free runtime state (health, active
// connections, ejection hold) and an immutable Snapshot handed to balancers.
package backend

import (
	"net/netip"
	"sync/atomic"
	"time"
)

// Endpoint is what discovery produces. It is a comparable value type.
type Endpoint struct {
	Address   netip.AddrPort
	Weight    int // 1..100
	Namespace string
	Service   string
	Node      string // Namespace, Service and Node are for logs only
}

// State is the health state of a backend.
type State uint8

const (
	Healthy State = iota
	Unhealthy
)

// Cause says which mechanism changed a backend's health.
type Cause uint8

const (
	CauseProbe   Cause = iota // active health check
	CausePassive              // passive ejection after a failed dial
)

// Backend is one dialable endpoint plus its runtime state. All state is held
// in atomics: it is read on every connection and written by health checkers
// concurrently. A Backend must not be copied after first use.
type Backend struct {
	ep      Endpoint
	addr    string
	maxConn int64

	unhealthy    atomic.Bool // zero value = healthy: new backends carry traffic at once
	cause        atomic.Uint32
	healthySince atomic.Int64 // unix nanos of the last transition to healthy (creation counts)
	ejectedUntil atomic.Int64 // unix nanos, 0 = no hold
	active       atomic.Int64
}

// New returns a healthy backend. maxConnections <= 0 means unlimited.
func New(ep Endpoint, maxConnections int) *Backend {
	if maxConnections < 0 {
		maxConnections = 0
	}
	b := &Backend{ep: ep, addr: ep.Address.String(), maxConn: int64(maxConnections)}
	b.healthySince.Store(time.Now().UnixNano())
	return b
}

// Endpoint returns the endpoint this backend was created from.
func (b *Backend) Endpoint() Endpoint { return b.ep }

// Address returns "ip:port" (IPv6 as "[ip]:port").
func (b *Backend) Address() string { return b.addr }

// IsHealthy reports whether the backend is currently healthy.
func (b *Backend) IsHealthy() bool { return !b.unhealthy.Load() }

// SetHealthy records a health verdict and reports whether it changed the state.
// The transition time is recorded; a transition to healthy restarts slow-start.
func (b *Backend) SetHealthy(healthy bool, cause Cause) (changed bool) {
	if !b.unhealthy.CompareAndSwap(healthy, !healthy) {
		return false
	}
	b.cause.Store(uint32(cause))
	if healthy {
		b.healthySince.Store(time.Now().UnixNano())
		b.ejectedUntil.Store(0)
	}
	return true
}

// LastCause returns the cause of the most recent health transition.
func (b *Backend) LastCause() Cause { return Cause(b.cause.Load()) } //nolint:gosec // G115: cause only ever stores a Cause value (uint8)

// State returns Healthy or Unhealthy.
func (b *Backend) State() State {
	if b.unhealthy.Load() {
		return Unhealthy
	}
	return Healthy
}

// HealthySince returns when the backend last became healthy (its creation time
// if it never flipped). Balancers use it for slow-start.
func (b *Backend) HealthySince() time.Time { return time.Unix(0, b.healthySince.Load()) }

// EjectedUntil returns the end of the passive ejection hold, or the zero time.
func (b *Backend) EjectedUntil() time.Time {
	n := b.ejectedUntil.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// SetEjectionHold sets the passive ejection hold to now+d. d <= 0 clears it.
func (b *Backend) SetEjectionHold(d time.Duration) {
	if d <= 0 {
		b.ejectedUntil.Store(0)
		return
	}
	b.ejectedUntil.Store(time.Now().Add(d).UnixNano())
}

// ActiveConnections returns the number of connections currently held.
func (b *Backend) ActiveConnections() int64 { return b.active.Load() }

// MaxConnections returns the per-backend cap; 0 means unlimited.
func (b *Backend) MaxConnections() int { return int(b.maxConn) }

// TryAcquire reserves a connection slot. It returns false when the backend is
// at its cap and never exceeds the cap under concurrency. Every true must be
// paired with exactly one Release.
func (b *Backend) TryAcquire() bool {
	if b.maxConn <= 0 {
		b.active.Add(1)
		return true
	}
	for {
		cur := b.active.Load()
		if cur >= b.maxConn {
			return false
		}
		if b.active.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release frees a slot taken by TryAcquire.
func (b *Backend) Release() { b.active.Add(-1) }

// Snapshot is an immutable view of a pool's backends. Healthy is the subset of
// All that was healthy when the snapshot was built; it does not track later
// health changes (a new Snapshot is built on every change). Never mutate the
// slices.
type Snapshot struct {
	All     []*Backend
	Healthy []*Backend
}

// NewSnapshot copies all and derives Healthy from the current health state.
func NewSnapshot(all []*Backend) *Snapshot {
	s := &Snapshot{All: append([]*Backend(nil), all...)}
	s.Healthy = make([]*Backend, 0, len(all))
	for _, b := range s.All {
		if b.IsHealthy() {
			s.Healthy = append(s.Healthy, b)
		}
	}
	return s
}
