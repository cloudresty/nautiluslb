// Package runtime is the lifecycle orchestrator: it owns the pools, the
// listeners, the discovery manager and the watchdog, and implements startup,
// graceful shutdown and hot reload with the ordering guarantees of the plan
// (D.1 to D.3).
//
// What main does around it: install signal handlers before New; call
// Shutdown, then close the access log (2s), then shut the admin server down
// (2s), so a final scrape still sees the drain metrics. A second signal during
// Shutdown is main cancelling the context it passed in, which makes the
// drains force-close promptly.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/emit"
	"k8s.io/client-go/kubernetes"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/discovery"
	"github.com/cloudresty/nautiluslb/internal/limits"
	"github.com/cloudresty/nautiluslb/internal/metrics"
	"github.com/cloudresty/nautiluslb/internal/pipe"
	"github.com/cloudresty/nautiluslb/internal/pool"
)

const defaultDrainTimeout = 30 * time.Second

// ErrNotRunning is returned by Reload before Start has finished or after
// Shutdown.
var ErrNotRunning = errors.New("runtime: not running")

// Options configures a Runtime.
type Options struct {
	// Config must be loaded, defaulted and validated (config.Load).
	Config    *config.Config
	Client    kubernetes.Interface
	Recorder  metrics.Recorder
	AccessLog accesslog.Logger
	// Dial replaces the backend dialer of every listener (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Sleep replaces the readiness-delay wait of Shutdown (tests). It must
	// return early when ctx is done.
	Sleep func(ctx context.Context, d time.Duration)
	// SyncTimeout is how long Start waits for discovery's first sync; 0 means
	// the discovery default (60s). Tests shorten it.
	SyncTimeout time.Duration

	trace func(step string) // tests: records lifecycle steps in order
}

// Summary reports what a Reload or Shutdown did.
type Summary struct {
	Added, Removed, Updated, Unchanged []string
	// Replaced lists configurations whose address or protocol changed: the new
	// listener was bound first, the old one is draining in the background.
	Replaced []string
	// Forced is the number of connections or sessions force-closed by a drain.
	Forced int
	// Ignored lists settings fields that changed but are not hot-reloadable.
	Ignored []string
}

const (
	stateNew = iota
	stateStarting
	stateRunning
	stateShutdown
)

// Runtime owns the whole data plane.
type Runtime struct {
	opts Options
	rec  metrics.Recorder
	alog accesslog.Logger

	settings config.Settings // effective; settings are not hot-reloadable
	gate     *limits.Gate    // global maxConnections, shared by every listener
	wd       *pipe.Watchdog
	disc     *discovery.Manager

	runCtx    context.Context // lives until the end of Shutdown; never the caller's ctx
	runCancel context.CancelFunc
	bgCtx     context.Context // background drains; cancelled when Shutdown's drain ctx ends
	bgCancel  context.CancelFunc

	mu          sync.Mutex // serialises Start, Reload and Shutdown; never taken by the sink
	state       int
	entries     map[string]*entry
	startCancel context.CancelFunc
	serveWG     sync.WaitGroup
	bg          sync.WaitGroup
	bgForced    atomic.Int64

	poolsMu sync.RWMutex // guards pools; the discovery sink reads it
	pools   map[string]*pool.Pool

	readyMu  sync.Mutex
	ready    bool
	reason   string
	serving  bool
	synced   bool
	stopping bool
}

// New builds every pool and listener of opts.Config and binds every listener.
// It is all-or-nothing: if any bind fails, the ones already bound are closed
// and an error is returned. It does not serve.
func New(opts Options) (*Runtime, error) {
	if opts.Config == nil {
		return nil, errors.New("runtime: nil configuration")
	}
	if opts.Client == nil {
		return nil, errors.New("runtime: nil kubernetes client")
	}
	rec := opts.Recorder
	if rec == nil {
		rec = metrics.NewNop()
	}
	alog := opts.AccessLog
	if alog == nil {
		alog = accesslog.Nop()
	}
	cfg := opts.Config

	r := &Runtime{
		opts:     opts,
		rec:      rec,
		alog:     alog,
		settings: cfg.Settings,
		gate:     limits.NewGate(cfg.Settings.Limits.MaxConnections),
		wd:       pipe.NewWatchdog(0, 0),
		entries:  make(map[string]*entry),
		pools:    make(map[string]*pool.Pool),
		reason:   "starting",
	}
	r.runCtx, r.runCancel = context.WithCancel(context.Background())
	r.bgCtx, r.bgCancel = context.WithCancel(context.Background())
	r.disc = discovery.NewManager(opts.Client, discovery.Options{
		Settings:    cfg.Settings.Discovery,
		Recorder:    rec,
		Sink:        sink{r},
		SyncTimeout: opts.SyncTimeout,
	})

	for _, c := range cfg.Configurations {
		for _, spec := range c.Pools() {
			p, err := pool.New(pool.Options{Spec: spec, Listener: c.Name, Recorder: rec})
			if err != nil {
				return nil, fmt.Errorf("configuration %q: pool %q: %w", c.Name, spec.Key, err)
			}
			r.pools[spec.Key] = p
		}
	}

	var built []*entry
	fail := func(err error) (*Runtime, error) {
		for _, e := range built {
			e.discard()
		}
		r.runCancel()
		r.bgCancel()
		return nil, err
	}
	for _, c := range cfg.Configurations {
		e, err := r.newEntry(c, r.pools)
		if err != nil {
			return fail(err)
		}
		built = append(built, e)
		if err := e.listen(); err != nil {
			return fail(fmt.Errorf("configuration %q: %w", c.Name, err))
		}
		r.entries[c.Name] = e
	}
	return r, nil
}

func (r *Runtime) trace(step string) {
	if r.opts.trace != nil {
		r.opts.trace(step)
	}
}

// sink routes discovery endpoints to pools; unknown keys are ignored.
type sink struct{ r *Runtime }

func (s sink) SetEndpoints(key string, eps []backend.Endpoint) {
	s.r.poolsMu.RLock()
	p := s.r.pools[key]
	s.r.poolsMu.RUnlock()
	if p != nil {
		p.SetEndpoints(eps)
	}
}

func (r *Runtime) markSynced() {
	r.readyMu.Lock()
	r.synced = true
	r.readyMu.Unlock()
	r.maybeReady()
}

// maybeReady flips ready once listeners serve and discovery has synced.
func (r *Runtime) maybeReady() {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	if r.stopping || r.ready || !r.serving || !r.synced {
		return
	}
	r.ready, r.reason = true, ""
	r.rec.Ready(true)
}

// Ready reports readiness and, when not ready, why. Readiness is a one-way
// latch until shutdown: once discovery has synced for the first time, a later
// Reload that adds a namespace whose informers have not synced yet does NOT
// make the runtime unready (pools of that namespace keep their endpoints and
// stay empty until it syncs); only Shutdown clears it.
func (r *Runtime) Ready() (bool, string) {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	return r.ready, r.reason
}

// Start starts the pools, the watchdog and discovery (waiting for the first
// sync, at most 60s), then serves every listener. ctx only bounds that wait:
// the pools, watchdog and discovery live until Shutdown, because they must
// outlive the signal context while connections drain. If discovery has not
// synced in time the listeners serve anyway (every connection gets no_backend
// until endpoints arrive) and readiness flips when it syncs. Start does not
// hold the runtime mutex while waiting, so Shutdown can interrupt it.
func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.state != stateNew {
		r.mu.Unlock()
		return errors.New("runtime: already started")
	}
	r.state = stateStarting
	startCtx, cancel := context.WithCancel(ctx)
	r.startCancel = cancel
	defer cancel()

	r.poolsMu.RLock()
	for _, p := range r.pools {
		p.Start(r.runCtx)
	}
	r.poolsMu.RUnlock()
	r.wd.Start(r.runCtx)
	specs := r.allSpecs(r.opts.Config.Configurations)
	r.mu.Unlock()

	// Flip ready when discovery reports its first full sync, including after
	// Start gave up waiting. Exits at the latest when Shutdown ends.
	go func() {
		select {
		case <-r.disc.Synced():
			r.markSynced()
		case <-r.runCtx.Done():
		}
	}()

	err := r.disc.Start(startCtx, specs)
	switch {
	case err == nil:
		r.markSynced()
	case errors.Is(err, discovery.ErrNotSynced):
		emit.Warn.Msg("Discovery not synced yet, serving without endpoints; readiness stays false until it syncs")
		r.setReason("discovery not synced")
	default:
		r.setReason("discovery failed")
		return fmt.Errorf("starting discovery: %w", err)
	}

	r.mu.Lock()
	if r.state != stateStarting {
		r.mu.Unlock()
		return errors.New("runtime: shut down during start")
	}
	for _, e := range r.entries {
		r.serveLocked(e)
	}
	r.state = stateRunning
	r.mu.Unlock()

	r.readyMu.Lock()
	r.serving = true
	r.readyMu.Unlock()
	r.maybeReady()
	return nil
}

// ListenerAddr returns the bound address of the named configuration's
// listener (useful when it was configured with port 0), and false when no
// such configuration exists.
func (r *Runtime) ListenerAddr(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[name]
	if !ok {
		return "", false
	}
	return e.addr(), true
}

func (r *Runtime) setReason(s string) {
	r.readyMu.Lock()
	if !r.ready {
		r.reason = s
	}
	r.readyMu.Unlock()
}

func (r *Runtime) serveLocked(e *entry) {
	r.serveWG.Add(1)
	go func() {
		defer r.serveWG.Done()
		e.serve()
	}()
}

func (r *Runtime) allSpecs(cfgs []config.Configuration) []config.PoolSpec {
	var specs []config.PoolSpec
	for _, c := range cfgs {
		specs = append(specs, c.Pools()...)
	}
	return specs
}

func (r *Runtime) drainTimeout() time.Duration {
	if d := r.settings.Drain.Timeout.Std(); d > 0 {
		return d
	}
	return defaultDrainTimeout
}

func (r *Runtime) sleep(ctx context.Context, d time.Duration) {
	if r.opts.Sleep != nil {
		r.opts.Sleep(ctx, d)
		return
	}
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Shutdown drains the runtime (plan D.2): ready=false, readiness delay,
// discovery stop, every listener drained concurrently within drain.timeout
// (or until ctx is cancelled, which forces the rest at once), pools stop,
// watchdog stop. It also drains and waits for listeners still draining from
// earlier reloads. It holds the runtime mutex, so it waits for a Reload in
// progress and a later Reload is refused. Calling it twice is harmless.
func (r *Runtime) Shutdown(ctx context.Context) Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateShutdown {
		return Summary{}
	}
	r.state = stateShutdown
	if r.startCancel != nil {
		r.startCancel()
	}

	r.readyMu.Lock()
	r.stopping, r.ready, r.reason = true, false, "shutting down"
	r.rec.Ready(false)
	r.readyMu.Unlock()
	r.trace("ready=false")

	r.sleep(ctx, r.settings.Drain.ReadinessDelay.Std())
	r.trace("readiness-delay")

	r.disc.Stop()
	r.trace("discovery.stop")

	dctx, cancel := context.WithTimeout(ctx, r.drainTimeout())
	defer cancel()
	stopBG := context.AfterFunc(dctx, r.bgCancel)
	defer stopBG()

	var forced atomic.Int64
	var wg sync.WaitGroup
	for _, e := range r.entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			forced.Add(int64(e.drain(dctx)))
		}()
	}
	wg.Wait()
	r.bg.Wait()
	total := int(forced.Load() + r.bgForced.Swap(0))
	r.trace("drain")
	if total > 0 {
		emit.Warn.StructuredFields("Force-closed connections at shutdown", emit.ZInt("forced", total))
	}

	r.poolsMu.RLock()
	for _, p := range r.pools {
		p.Stop()
	}
	r.poolsMu.RUnlock()
	r.trace("pools.stop")
	r.wd.Stop()
	r.trace("watchdog.stop")
	r.serveWG.Wait()
	r.runCancel()
	r.bgCancel()
	return Summary{Forced: total}
}
