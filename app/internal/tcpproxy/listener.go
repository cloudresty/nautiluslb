// Package tcpproxy is the TCP/TLS data path: accept, admit (ACL, limits),
// optional inbound PROXY and SNI peek, backend selection and dial, and the
// byte copy (internal/pipe).
package tcpproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/limits"
	"github.com/cloudresty/nautiluslb/internal/metrics"
	"github.com/cloudresty/nautiluslb/internal/pipe"
)

const (
	// dialBudget is how many distinct backends are tried for one client.
	dialBudget = 3

	// proxyReadTimeout bounds waiting for an inbound PROXY header.
	proxyReadTimeout = 5 * time.Second

	// proxyWriteTimeout bounds writing the outbound PROXY header.
	proxyWriteTimeout = 2 * time.Minute

	// forceWait bounds how long Drain waits for handlers after a force-close.
	forceWait = 5 * time.Second
)

// dialKeepAlive detects a peer that vanished without closing. A dead peer is
// noticed after Idle + Interval*Count = 60s; it never closes a live one.
var dialKeepAlive = net.KeepAliveConfig{
	Enable:   true,
	Idle:     30 * time.Second,
	Interval: 10 * time.Second,
	Count:    3,
}

// ErrAddressChanged is returned by Update when the listener address or
// protocol differs: that needs a new Listener, not an in-place update.
var ErrAddressChanged = errors.New("tcpproxy: listener address or protocol changed")

var copyBuffers = &sync.Pool{New: func() any {
	b := make([]byte, 32*1024)
	return &b
}}

// Pool is what a Listener needs from a backend pool. *pool.Pool satisfies it.
// (Deviation from the plan, which names the concrete type: the small
// interface lets tests use fakes and keeps tcpproxy independent of pool's
// construction.)
type Pool interface {
	Key() string
	Pick(key balancer.Key, n int) []*backend.Backend
	Eject(b *backend.Backend)
}

// Options configures a Listener.
type Options struct {
	Config    config.Configuration
	Pools     map[string]Pool // by pool key: "<config>" or "<config>/<route>"
	Global    *limits.Gate    // optional, shared by every listener
	Recorder  metrics.Recorder
	AccessLog accesslog.Logger
	Watchdog  *pipe.Watchdog
	Dial      func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Listener serves one tcp or tls configuration.
type Listener struct {
	name    string
	address string // configured address, compared by Update
	proto   config.Protocol

	rec     metrics.Recorder
	alog    accesslog.Logger
	wd      *pipe.Watchdog
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	global  *limits.Gate
	gate    *limits.Gate
	perSrc  *limits.PerSource
	rc      atomic.Pointer[runtimeConfig]
	updMu   sync.Mutex
	reg     *registry
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex // guards ln, started, closing; never on the per-connection path
	ln       net.Listener
	started  bool
	served   chan struct{}
	closing  chan struct{}
	closeOne sync.Once
	draining atomic.Bool
}

// New builds a Listener: it parses ACL, PROXY CIDRs, the SNI router and the
// limits, and binds nothing.
func New(opts Options) (*Listener, error) {
	cfg := opts.Config
	pools := opts.Pools
	if pools == nil {
		pools = map[string]Pool{}
	}
	rc, err := buildRuntimeConfig(cfg, pools)
	if err != nil {
		return nil, fmt.Errorf("listener %q: %w", cfg.Name, err)
	}

	l := &Listener{
		name:    cfg.Name,
		address: cfg.ListenerAddress,
		proto:   normProtocol(cfg.Protocol),
		rec:     opts.Recorder,
		alog:    opts.AccessLog,
		wd:      opts.Watchdog,
		dial:    opts.Dial,
		global:  opts.Global,
		gate:    limits.NewGate(cfg.Limits.MaxConnections),
		perSrc:  limits.NewPerSource(cfg.Limits.MaxConnectionsPerSource),
		reg:     newRegistry(),
		served:  make(chan struct{}),
		closing: make(chan struct{}),
	}
	if l.rec == nil {
		l.rec = metrics.NewNop()
	}
	if l.alog == nil {
		l.alog = accesslog.Nop()
	}
	if l.dial == nil {
		d := &net.Dialer{KeepAliveConfig: dialKeepAlive}
		l.dial = d.DialContext
	}
	l.baseCtx, l.cancel = context.WithCancel(context.Background())
	l.rc.Store(rc)
	return l, nil
}

// Name returns the configuration name.
func (l *Listener) Name() string { return l.name }

// Address returns the bound address once Listen succeeded, else the
// configured one.
func (l *Listener) Address() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln != nil {
		return l.ln.Addr().String()
	}
	return l.address
}

// Active returns the number of registered connections.
func (l *Listener) Active() int { return int(l.reg.active.Load()) }

// Listen binds the listening socket without serving.
func (l *Listener) Listen() error {
	lc := net.ListenConfig{KeepAliveConfig: dialKeepAlive}
	ln, err := lc.Listen(context.Background(), "tcp", l.address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", l.address, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.closing:
		_ = ln.Close()
		return nil
	default:
	}
	l.ln = ln
	return nil
}

// Serve accepts connections until the listener is closed (Drain). It does
// nothing if Listen bound none.
func (l *Listener) Serve() {
	l.mu.Lock()
	ln := l.ln
	select {
	case <-l.closing:
		ln = nil
	default:
	}
	if ln == nil || l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.mu.Unlock()
	l.serveOn(ln)
}

func (l *Listener) serveOn(ln net.Listener) {
	defer close(l.served)

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				emit.Info.StructuredFields("Listener closed",
					emit.ZString("listener", l.name))
				return
			}

			// Typically EMFILE. Back off rather than spin on the error.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			emit.Error.StructuredFields("Failed to accept connection",
				emit.ZString("listener", l.name),
				emit.ZString("error", err.Error()),
				emit.ZString("retry_in", backoff.String()))

			t := time.NewTimer(backoff)
			select {
			case <-l.closing:
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		backoff = 0

		if l.draining.Load() {
			l.rejectEarly(conn, "draining", accesslog.ResultDraining)
			continue
		}

		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(conn)
		}()
	}
}

// Update applies cfg in place: ACL, limits, PROXY, SNI routes, pools and
// timeouts. In-flight connections keep the settings they started with.
func (l *Listener) Update(cfg config.Configuration, pools map[string]Pool) error {
	if cfg.ListenerAddress != l.address || normProtocol(cfg.Protocol) != l.proto {
		return ErrAddressChanged
	}
	if pools == nil {
		pools = map[string]Pool{}
	}
	rc, err := buildRuntimeConfig(cfg, pools)
	if err != nil {
		return fmt.Errorf("listener %q: %w", l.name, err)
	}

	l.updMu.Lock()
	defer l.updMu.Unlock()
	l.gate.SetCapacity(cfg.Limits.MaxConnections)
	l.perSrc.SetCapacity(cfg.Limits.MaxConnectionsPerSource)
	l.rc.Store(rc)
	return nil
}

// Drain closes the listening socket, waits for connections to finish until
// ctx is done, then force-closes the rest and returns how many it forced.
func (l *Listener) Drain(ctx context.Context) (forced int) {
	l.mu.Lock()
	l.draining.Store(true)
	l.closeOne.Do(func() { close(l.closing) })
	ln, started := l.ln, l.started
	l.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	if started {
		<-l.served // Serve exits promptly: Accept fails and backoff selects on closing.
	}

	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		l.cancel() // abort dials still in flight; handlers registering now see it
		forced = l.reg.closeAll()
		select {
		case <-done:
		case <-time.After(forceWait):
			emit.Warn.StructuredFields("Connections still running after forced close",
				emit.ZString("listener", l.name),
				emit.ZInt("active", l.Active()))
		}
	}
	l.cancel()
	return forced
}
