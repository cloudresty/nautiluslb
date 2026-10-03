// Package pool owns the backends of one pool: discovery writes endpoints into
// it, the data path picks from it without locking.
//
// Concurrency design. SetEndpoints, Update, Eject-driven refreshes and health
// transitions are serialised by one mutex and publish an immutable view (a
// backend.Snapshot plus a pre-built fail-open snapshot) through an
// atomic.Pointer; Pick and Snapshot only do atomic loads.
//
// Health transitions reach Pick like this: backend.Snapshot.Healthy is
// immutable, so every transition must publish a new view. The health.Checker is
// handed health.Options.OnTransition = refresh, which the checker calls after
// every SetHealthy that changed state (probe up/down, passive recovery, Eject:
// one transition site, metrics are not involved). refresh rebuilds the snapshot from the live IsHealthy
// flags and rebuilds the picker. A reader that races a transition can see the
// old view for the few microseconds before the refresh lands.
package pool

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/cloudresty/emit"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/health"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// Options configures a Pool.
type Options struct {
	Spec     config.PoolSpec
	Listener string
	Recorder metrics.Recorder
	// Dial is reserved for tests; the pool itself does not dial.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

type view struct {
	snap *backend.Snapshot
	open *backend.Snapshot // snap.All as Healthy; only set when snap.Healthy is empty
}

type pickerBox struct{ p balancer.Picker }

// Pool is one backend pool.
type Pool struct {
	listener string
	key      string
	rec      metrics.Recorder
	checker  *health.Checker

	cur    atomic.Pointer[view]
	picker atomic.Pointer[pickerBox]
	open   atomic.Bool // currently failing open (warn-once state)

	mu       sync.Mutex // guards the fields below and serialises publishes
	spec     config.PoolSpec
	backends []*backend.Backend
}

// New builds a Pool. It returns balancer.ErrUnknownAlgorithm for a bad algorithm.
func New(opts Options) (*Pool, error) {
	pk, err := newPicker(opts.Spec)
	if err != nil {
		return nil, err
	}
	rec := opts.Recorder
	if rec == nil {
		rec = metrics.NewNop()
	}
	p := &Pool{listener: opts.Listener, key: opts.Spec.Key, rec: rec, spec: opts.Spec}
	p.picker.Store(&pickerBox{pk})
	p.checker = health.NewChecker(p.checkerOptions(opts.Spec))
	empty := backend.NewSnapshot(nil)
	p.cur.Store(&view{snap: empty})
	return p, nil
}

func newPicker(spec config.PoolSpec) (balancer.Picker, error) {
	return balancer.New(spec.Balancer.Algorithm, balancer.Options{SlowStart: spec.Balancer.SlowStart.Std()})
}

func (p *Pool) checkerOptions(spec config.PoolSpec) health.Options {
	h := spec.Health
	var prober health.Prober
	switch h.Type {
	case config.HealthTCP:
		prober = health.NewTCPProber()
	case config.HealthHTTP:
		prober = health.NewHTTPProber(h.HTTP.Path, h.HTTP.Host, h.HTTP.ExpectStatus)
	}
	if prober != nil && h.Port > 0 {
		prober = portProber{inner: prober, port: uint16(h.Port)} //nolint:gosec // G115: config validation bounds health.port to 1..65535
	}
	return health.Options{
		Interval:     h.Interval.Std(),
		Timeout:      h.Timeout.Std(),
		EjectionHold: h.EjectionHold.Std(),
		Rise:         h.Rise,
		Fall:         h.Fall,
		Jitter:       h.Jitter,
		Prober:       prober,
		Recorder:     p.rec,
		OnTransition: func(*backend.Backend, bool, backend.Cause) { p.refresh() },
		Listener:     p.listener,
		Pool:         p.key,
	}
}

// portProber probes the backend's IP on a fixed port (health.port override).
type portProber struct {
	inner health.Prober
	port  uint16
}

func (pp portProber) Probe(ctx context.Context, addr string) error {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		addr = netip.AddrPortFrom(ap.Addr(), pp.port).String()
	} else if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = net.JoinHostPort(host, strconv.Itoa(int(pp.port)))
	}
	return pp.inner.Probe(ctx, addr)
}

// Key returns the pool key.
func (p *Pool) Key() string { return p.key }

// Start begins health checking under ctx.
func (p *Pool) Start(ctx context.Context) { p.checker.Start(ctx) }

// Stop stops health checking and waits for the probe loops to exit.
func (p *Pool) Stop() { p.checker.Stop() }

// Snapshot returns the current immutable snapshot. Lock-free.
func (p *Pool) Snapshot() *backend.Snapshot { return p.cur.Load().snap }

// SetEndpoints is the single writer of the backend set. A backend is kept
// (same *Backend: health, counters and probe loop survive) when an endpoint
// with the same address, the same weight and the pool's current connection cap
// exists. Endpoint is immutable, so a changed weight (or a cap changed by
// Update) replaces the object and its health state restarts as healthy.
// Duplicate addresses: the first wins.
func (p *Pool) SetEndpoints(eps []backend.Endpoint) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := make(map[string]*backend.Backend, len(p.backends))
	for _, b := range p.backends {
		old[b.Address()] = b
	}
	cap := p.spec.MaxConnectionsPerBackend
	next := make([]*backend.Backend, 0, len(eps))
	seen := make(map[string]struct{}, len(eps))
	for _, ep := range eps {
		addr := ep.Address.String()
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		if b, ok := old[addr]; ok && b.Endpoint().Weight == ep.Weight && b.MaxConnections() == max(cap, 0) {
			next = append(next, b)
			continue
		}
		next = append(next, backend.New(ep, cap))
	}
	p.backends = next
	p.publishLocked()
	p.checker.Reconcile(next)
}

// refresh republishes the snapshot after a health transition.
func (p *Pool) refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishLocked()
}

func (p *Pool) publishLocked() {
	snap := backend.NewSnapshot(p.backends)
	v := &view{snap: snap}
	if len(snap.Healthy) == 0 && len(snap.All) > 0 {
		v.open = &backend.Snapshot{All: snap.All, Healthy: snap.All}
	}
	pk := p.picker.Load().p
	if v.open != nil {
		pk.Rebuild(v.open)
	} else {
		pk.Rebuild(snap)
		p.open.Store(false) // healthy backends exist again: re-arm the warning
	}
	p.cur.Store(v)
	p.rec.PoolSize(p.listener, p.key, len(snap.Healthy), len(snap.All))
}

// Pick returns up to n candidates, healthy first. When no backend is healthy it
// fails open to the whole set (PR #8 invariant) and logs once per transition.
// The returned slice is shared and must not be modified.
func (p *Pool) Pick(key balancer.Key, n int) []*backend.Backend {
	v := p.cur.Load()
	pk := p.picker.Load().p
	if len(v.snap.Healthy) > 0 {
		return pk.Pick(v.snap, key, n)
	}
	if v.open == nil {
		return nil
	}
	if p.open.CompareAndSwap(false, true) {
		emit.Warn.StructuredFields("No healthy backends, failing open to all backends",
			emit.ZString("listener", p.listener),
			emit.ZString("pool", p.key),
			emit.ZInt("backends", len(v.snap.All)))
	}
	return pk.Pick(v.open, key, n)
}

// Eject passively marks b unhealthy for the configured ejection hold (after a
// failed dial). The checker owns the transition (hold, flag, metrics) and
// calls OnTransition, which refreshes the pool.
func (p *Pool) Eject(b *backend.Backend) {
	p.mu.Lock()
	hold := p.spec.Health.EjectionHold.Std()
	p.mu.Unlock()
	p.checker.Eject(b, hold)
}

// Update applies a hot-reloaded spec: a new picker when the algorithm or
// slow-start changed, new checker options, and the new connection cap for
// backends created from now on (existing backends keep their cap until
// SetEndpoints next replaces them). Endpoints are untouched.
func (p *Pool) Update(spec config.PoolSpec) error {
	pk, err := newPicker(spec)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if spec.Balancer != p.spec.Balancer {
		p.picker.Store(&pickerBox{pk})
	}
	p.spec = spec
	p.checker.Update(p.checkerOptions(spec))
	p.publishLocked()
	p.checker.Reconcile(p.backends)
	return nil
}
