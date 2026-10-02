// Package udpproxy is the UDP listener: it keeps one session per client
// address, each with its own connected socket to a backend picked once, so
// every datagram of a flow reaches the same backend.
package udpproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/acl"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/limits"
	"github.com/cloudresty/nautiluslb/internal/metrics"
	"github.com/cloudresty/nautiluslb/internal/neterr"
)

// ErrAddressChanged is returned by Update when the listener address or protocol differ.
var ErrAddressChanged = errors.New("udpproxy: listener address or protocol changed")

const (
	shardCount   = 16
	writeTimeout = 50 * time.Millisecond
	socketBuffer = 4 << 20
	defaultIdle  = 60 * time.Second
	defaultBuf   = 65535
	dialBudget   = 3 // candidates tried per new session, as in tcpproxy
)

// Pool is the part of *pool.Pool the proxy uses. Defined here (consumer side)
// so tests can use fakes; *pool.Pool satisfies it. Pick results are shared
// read-only slices.
type Pool interface {
	Key() string
	Pick(key balancer.Key, n int) []*backend.Backend
	Eject(b *backend.Backend)
}

// Options configures a Listener.
type Options struct {
	Config    config.Configuration
	Pools     map[string]Pool // by pool key; a UDP listener uses Pools[Config.Name]
	Global    *limits.Gate    // process-wide session gate, may be nil
	Recorder  metrics.Recorder
	AccessLog accesslog.Logger
	// Dial opens the connected backend socket. nil = net.DialUDP.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

type runtimeConfig struct {
	acl     *acl.List
	pool    Pool
	idle    time.Duration
	bufSize int
}

type shard struct {
	mu sync.Mutex
	m  map[netip.AddrPort]*session
}

// Listener is one UDP listener.
type Listener struct {
	name, address string
	rec           metrics.Recorder
	alog          accesslog.Logger
	global        *limits.Gate
	dial          func(ctx context.Context, network, addr string) (net.Conn, error)

	rc        atomic.Pointer[runtimeConfig]
	gate      *limits.Gate
	perSource *limits.PerSource

	shards [shardCount]shard

	mu       sync.Mutex
	conn     *net.UDPConn
	closed   bool
	started  bool
	serveEnd chan struct{}

	draining atomic.Bool
	wg       sync.WaitGroup // session reader goroutines
	bufs     sync.Pool
}

type session struct {
	l       *Listener
	client  netip.AddrPort // as received (may be 4in6)
	key     netip.AddrPort
	b       *backend.Backend
	pool    Pool
	conn    net.Conn
	start   time.Time
	last    atomic.Int64
	in, out atomic.Int64
	dIn     atomic.Int64
	dOut    atomic.Int64
	closed  atomic.Bool
	gates   struct{ global, listener bool }
}

func buildRC(cfg config.Configuration, pools map[string]Pool) (*runtimeConfig, error) {
	list, err := acl.Parse(cfg.Access.Allow, cfg.Access.Deny)
	if err != nil {
		return nil, fmt.Errorf("parsing access lists: %w", err)
	}
	p, ok := pools[cfg.Name]
	if !ok || p == nil {
		return nil, fmt.Errorf("no pool for listener %q", cfg.Name)
	}
	rc := &runtimeConfig{acl: list, pool: p, idle: defaultIdle, bufSize: defaultBuf}
	if cfg.UDP != nil {
		if d := cfg.UDP.SessionIdleTimeout.Std(); d > 0 {
			rc.idle = d
		}
		if cfg.UDP.BufferSize > 0 {
			rc.bufSize = cfg.UDP.BufferSize
		}
	}
	return rc, nil
}

func udpLimits(cfg config.Configuration) (maxSessions, perSource int) {
	if cfg.UDP == nil {
		return 0, 0
	}
	return cfg.UDP.MaxSessions, cfg.UDP.MaxSessionsPerSource
}

// New validates the configuration and builds a Listener; it binds nothing.
func New(opts Options) (*Listener, error) {
	rc, err := buildRC(opts.Config, opts.Pools)
	if err != nil {
		return nil, fmt.Errorf("udpproxy %s: %w", opts.Config.Name, err)
	}
	ms, ps := udpLimits(opts.Config)
	l := &Listener{
		name:      opts.Config.Name,
		address:   opts.Config.ListenerAddress,
		rec:       opts.Recorder,
		alog:      opts.AccessLog,
		global:    opts.Global,
		dial:      opts.Dial,
		gate:      limits.NewGate(ms),
		perSource: limits.NewPerSource(ps),
		serveEnd:  make(chan struct{}),
	}
	if l.rec == nil {
		l.rec = metrics.NewNop()
	}
	if l.alog == nil {
		l.alog = accesslog.Nop()
	}
	if l.dial == nil {
		l.dial = dialUDP
	}
	for i := range l.shards {
		l.shards[i].m = make(map[netip.AddrPort]*session)
	}
	l.rc.Store(rc)
	return l, nil
}

func dialUDP(ctx context.Context, network, addr string) (net.Conn, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return net.DialUDP(network, nil, net.UDPAddrFromAddrPort(ap))
}

// Name returns the configuration name.
func (l *Listener) Name() string { return l.name }

// Address returns the configured listener address.
func (l *Listener) Address() string { return l.address }

// LocalAddr returns the bound address, or nil before Listen.
func (l *Listener) LocalAddr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil
	}
	return l.conn.LocalAddr()
}

// Active returns the number of live sessions.
func (l *Listener) Active() int { return int(l.gate.Active()) }

// Listen binds the UDP socket.
func (l *Listener) Listen() error {
	ua, err := net.ResolveUDPAddr("udp", l.address)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", l.address, err)
	}
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return fmt.Errorf("listening udp %s: %w", l.address, err)
	}
	_ = c.SetReadBuffer(socketBuffer)
	_ = c.SetWriteBuffer(socketBuffer)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = c.Close()
		return net.ErrClosed
	}
	l.conn = c
	return nil
}

// Update applies a new configuration in place. Address and protocol must be unchanged.
func (l *Listener) Update(cfg config.Configuration, pools map[string]Pool) error {
	if cfg.ListenerAddress != l.address || cfg.Protocol != config.ProtocolUDP {
		return ErrAddressChanged
	}
	rc, err := buildRC(cfg, pools)
	if err != nil {
		return fmt.Errorf("udpproxy %s: %w", l.name, err)
	}
	ms, ps := udpLimits(cfg)
	l.gate.SetCapacity(ms)
	l.perSource.SetCapacity(ps)
	l.rc.Store(rc)
	return nil
}

func (l *Listener) getBuf(n int) *[]byte {
	if v, ok := l.bufs.Get().(*[]byte); ok && cap(*v) >= n {
		*v = (*v)[:n]
		return v
	}
	b := make([]byte, n)
	return &b
}

func (l *Listener) shardFor(k netip.AddrPort) *shard {
	b := k.Addr().As16()
	h := uint32(2166136261) ^ uint32(k.Port())
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return &l.shards[h%shardCount]
}

// Serve runs the read loop and the sweeper; it returns when the socket is
// closed, after every session has ended.
func (l *Listener) Serve() {
	l.mu.Lock()
	c := l.conn
	if c == nil || l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.mu.Unlock()
	defer close(l.serveEnd)

	stop := make(chan struct{})
	var sw sync.WaitGroup
	sw.Add(1)
	go func() { defer sw.Done(); l.sweepLoop(stop) }()

	buf := make([]byte, l.rc.Load().bufSize)
	for {
		if want := l.rc.Load().bufSize; want != len(buf) {
			buf = make([]byte, want)
		}
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		l.handle(c, from, buf[:n])
	}
	close(stop)
	sw.Wait()
	l.closeAll("draining")
	l.wg.Wait()
}

func (l *Listener) handle(c *net.UDPConn, from netip.AddrPort, data []byte) {
	key := netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	sh := l.shardFor(key)
	sh.mu.Lock()
	s := sh.m[key]
	sh.mu.Unlock()
	if s != nil {
		s.forward(data)
		return
	}
	l.open(c, from, key, data)
}

func (l *Listener) reject(reason string) { l.rec.ConnRejected(l.name, reason) }

func (l *Listener) open(c *net.UDPConn, from, key netip.AddrPort, data []byte) {
	if l.draining.Load() {
		l.reject("draining")
		return
	}
	rc := l.rc.Load()
	ip := key.Addr()
	if !rc.acl.Allowed(ip) {
		l.reject("acl")
		return
	}
	s := &session{l: l, client: from, key: key, pool: rc.pool, start: time.Now()}
	if l.global != nil {
		if !l.global.TryAcquire() {
			l.reject("limit_global")
			return
		}
		s.gates.global = true
	}
	if !l.gate.TryAcquire() {
		s.releaseGates(false)
		l.reject("limit_listener")
		return
	}
	s.gates.listener = true
	if !l.perSource.TryAcquire(ip) {
		s.releaseGates(false)
		l.reject("limit_source")
		return
	}
	cands := rc.pool.Pick(balancer.Key{SourceIP: ip}, dialBudget)
	if len(cands) == 0 {
		s.releaseGates(true)
		l.reject("no_backend")
		return
	}
	var (
		b      *backend.Backend
		bc     net.Conn
		reason = "limit_backend" // every candidate at its cap
	)
	for _, cand := range cands {
		if !cand.TryAcquire() {
			continue // at its session cap: try the next candidate
		}
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := l.dial(ctx, "udp", cand.Address())
		cancel()
		if err != nil {
			cause := neterr.Classify(err)
			l.rec.BackendDial(l.name, rc.pool.Key(), cand.Address(), cause.String(), time.Since(t0))
			if neterr.BackendAttributable(cause) {
				rc.pool.Eject(cand)
			}
			cand.Release()
			reason = "dial_failed"
			continue
		}
		l.rec.BackendDial(l.name, rc.pool.Key(), cand.Address(), "ok", time.Since(t0))
		b, bc = cand, conn
		break
	}
	if b == nil {
		s.releaseGates(true)
		l.reject(reason)
		return
	}
	l.rec.BackendActive(l.name, rc.pool.Key(), b.Address(), 1)
	s.b, s.conn = b, bc
	s.last.Store(time.Now().UnixNano())

	sh := l.shardFor(key)
	sh.mu.Lock()
	sh.m[key] = s
	sh.mu.Unlock()
	l.rec.UDPSession(l.name, "open")

	bp := l.getBuf(rc.bufSize)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.bufs.Put(bp)
		s.readLoop(c, *bp)
	}()
	s.forward(data)
}

func (s *session) releaseGates(perSource bool) {
	if perSource {
		s.l.perSource.Release(s.key.Addr())
	}
	if s.gates.listener {
		s.l.gate.Release()
	}
	if s.gates.global {
		s.l.global.Release()
	}
}

// forward sends one client datagram to the backend. Never blocks for long.
func (s *session) forward(data []byte) {
	if s.closed.Load() {
		return
	}
	s.last.Store(time.Now().UnixNano())
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	n, err := s.conn.Write(data)
	if err != nil {
		if s.closed.Load() {
			return
		}
		cause := neterr.Classify(err)
		if cause == neterr.CauseRefused || cause == neterr.CauseUnreachable {
			s.pool.Eject(s.b)
			s.end("backend_reset", "error", err)
		}
		return // otherwise: datagram dropped
	}
	s.in.Add(int64(n))
	s.dIn.Add(1)
	s.l.rec.UDPDatagram(s.l.name, "in", n)
}

// readLoop relays backend replies to the client until the socket is closed.
func (s *session) readLoop(lc *net.UDPConn, buf []byte) {
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			if s.closed.Load() {
				return
			}
			cause := neterr.Classify(err)
			if cause == neterr.CauseRefused || cause == neterr.CauseUnreachable {
				s.pool.Eject(s.b)
			}
			s.end("backend_reset", "error", err)
			return
		}
		s.last.Store(time.Now().UnixNano())
		if _, err := lc.WriteToUDPAddrPort(buf[:n], s.client); err != nil {
			if errors.Is(err, net.ErrClosed) {
				s.end("ok", "expired", nil)
				return
			}
			s.l.reject("write_error") // reply dropped; see forward
			continue
		}
		s.out.Add(int64(n))
		s.dOut.Add(1)
		s.l.rec.UDPDatagram(s.l.name, "out", n)
	}
}

// end finishes the session exactly once: unregister, close the socket (which
// unblocks the reader), release gates and backend, emit one closing event and
// one access-log record.
func (s *session) end(result, event string, cause error) {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	l := s.l
	sh := l.shardFor(s.key)
	sh.mu.Lock()
	if sh.m[s.key] == s {
		delete(sh.m, s.key)
	}
	sh.mu.Unlock()
	_ = s.conn.Close()
	s.b.Release()
	l.rec.BackendActive(l.name, s.pool.Key(), s.b.Address(), -1)
	s.releaseGates(true)
	l.rec.UDPSession(l.name, event)
	rec := accesslog.Record{
		Time:     s.start,
		Listener: l.name,
		Protocol: "udp",
		Pool:     s.pool.Key(),
		Backend:  s.b.Address(),
		Client:   s.client.String(),
		Local:    l.address,
		Duration: time.Since(s.start),
		BytesIn:  s.in.Load(),
		BytesOut: s.out.Load(),
		Result:   result,
	}
	if cause != nil {
		rec.Error = cause.Error()
	}
	l.alog.Log(rec)
}

func (l *Listener) sweepLoop(stop <-chan struct{}) {
	t := time.NewTimer(l.sweepEvery())
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			l.sweep(time.Now())
			t.Reset(l.sweepEvery())
		}
	}
}

func (l *Listener) sweepEvery() time.Duration {
	return max(l.rc.Load().idle/4, time.Millisecond)
}

func (l *Listener) sweep(now time.Time) {
	idle := l.rc.Load().idle
	var expired []*session
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		for _, s := range sh.m {
			if now.Sub(time.Unix(0, s.last.Load())) >= idle {
				expired = append(expired, s)
			}
		}
		sh.mu.Unlock()
	}
	for _, s := range expired {
		s.end("ok", "expired", nil)
	}
}

// closeAll ends every session and returns how many this call ended.
func (l *Listener) closeAll(result string) int {
	var all []*session
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		for _, s := range sh.m {
			all = append(all, s)
		}
		sh.mu.Unlock()
	}
	n := 0
	for _, s := range all {
		if !s.closed.Load() {
			n++
		}
		s.end(result, "expired", nil)
	}
	return n
}

// Drain stops accepting new sessions while existing ones keep forwarding, waits
// until they idle out or ctx ends, then closes everything and the socket. It
// returns the number of sessions that were force-closed.
func (l *Listener) Drain(ctx context.Context) (forced int) {
	l.draining.Store(true)
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
wait:
	for l.Active() > 0 {
		select {
		case <-ctx.Done():
			break wait
		case <-t.C:
		}
	}
	forced = l.closeAll("draining")
	l.mu.Lock()
	l.closed = true
	c, started := l.conn, l.started
	l.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	if started {
		<-l.serveEnd
	}
	return forced
}
