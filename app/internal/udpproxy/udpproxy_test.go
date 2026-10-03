package udpproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/limits"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

type fakePool struct {
	mu     sync.Mutex
	bs     []*backend.Backend
	next   int
	ejects []*backend.Backend
	all    bool // Pick returns every backend (in order) instead of round-robin one
}

func (p *fakePool) Key() string { return "dns" }
func (p *fakePool) Pick(_ balancer.Key, _ int) []*backend.Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bs) == 0 {
		return nil
	}
	if p.all {
		return append([]*backend.Backend(nil), p.bs...)
	}
	b := p.bs[p.next%len(p.bs)]
	p.next++
	return []*backend.Backend{b}
}
func (p *fakePool) Eject(b *backend.Backend) {
	p.mu.Lock()
	p.ejects = append(p.ejects, b)
	p.mu.Unlock()
}
func (p *fakePool) ejected() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.ejects) }

type fakeRec struct {
	metrics.Recorder
	open, closing atomic.Int64
	mu            sync.Mutex
	rejected      map[string]int
	drained       []int
}

func newRec() *fakeRec { return &fakeRec{Recorder: metrics.NewNop(), rejected: map[string]int{}} }
func (r *fakeRec) UDPSession(_, ev string) {
	if ev == "open" {
		r.open.Add(1)
	} else {
		r.closing.Add(1)
	}
}
func (r *fakeRec) ConnRejected(_, reason string) { r.mu.Lock(); r.rejected[reason]++; r.mu.Unlock() }
func (r *fakeRec) DrainForced(_ string, n int) {
	r.mu.Lock()
	r.drained = append(r.drained, n)
	r.mu.Unlock()
}
func (r *fakeRec) rej(reason string) int { r.mu.Lock(); defer r.mu.Unlock(); return r.rejected[reason] }

type fakeLog struct {
	mu   sync.Mutex
	recs []accesslog.Record
}

func (f *fakeLog) Log(r accesslog.Record) { f.mu.Lock(); f.recs = append(f.recs, r); f.mu.Unlock() }
func (f *fakeLog) all() []accesslog.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]accesslog.Record(nil), f.recs...)
}

// echo starts a UDP server that replies with tag+payload; stop closes it.
func echo(t *testing.T, tag string) (netip.AddrPort, func()) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDPAddrPort(append([]byte(tag), buf[:n]...), from)
		}
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = c.Close()
			<-done
		}
	}
	t.Cleanup(stop)
	return c.LocalAddr().(*net.UDPAddr).AddrPort(), stop
}

func newBackend(ap netip.AddrPort) *backend.Backend {
	return backend.New(backend.Endpoint{Address: ap, Weight: 1}, 0)
}

func baseCfg() config.Configuration {
	return config.Configuration{
		Name: "dns", Protocol: config.ProtocolUDP, ListenerAddress: "127.0.0.1:0",
		UDP: &config.UDP{SessionIdleTimeout: config.Duration(time.Minute), BufferSize: 2048},
	}
}

type harness struct {
	l    *Listener
	pool *fakePool
	rec  *fakeRec
	log  *fakeLog
	addr *net.UDPAddr
	done chan struct{}
}

func start(t *testing.T, cfg config.Configuration, pool *fakePool, global *limits.Gate) *harness {
	t.Helper()
	h := &harness{pool: pool, rec: newRec(), log: &fakeLog{}, done: make(chan struct{})}
	l, err := New(Options{Config: cfg, Pools: map[string]Pool{"dns": pool}, Global: global, Recorder: h.rec, AccessLog: h.log})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Listen(); err != nil {
		t.Fatal(err)
	}
	h.l = l
	h.addr = l.LocalAddr().(*net.UDPAddr)
	go func() { l.Serve(); close(h.done) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		l.Drain(ctx)
		<-h.done
	})
	return h
}

func (h *harness) client(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.DialUDP("udp", nil, h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func roundTrip(t *testing.T, c *net.UDPConn, msg string) string {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read reply to %q: %v", msg, err)
	}
	return string(buf[:n])
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEchoRoundTrip(t *testing.T) {
	a, _ := echo(t, "A:")
	h := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	c := h.client(t)
	if got := roundTrip(t, c, "hello"); got != "A:hello" {
		t.Fatalf("got %q", got)
	}
	if h.l.Active() != 1 {
		t.Fatalf("active = %d", h.l.Active())
	}
}

func TestSessionAffinity(t *testing.T) {
	a, _ := echo(t, "A:")
	b, _ := echo(t, "B:")
	h := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{newBackend(a), newBackend(b)}}, nil)
	c1, c2 := h.client(t), h.client(t)
	first1 := roundTrip(t, c1, "x")[:2]
	first2 := roundTrip(t, c2, "x")[:2]
	if first1 == first2 {
		t.Fatalf("two clients landed on same backend %q (round-robin expected)", first1)
	}
	for i := 0; i < 50; i++ {
		if got := roundTrip(t, c1, "y")[:2]; got != first1 {
			t.Fatalf("c1 moved %q -> %q", first1, got)
		}
		if got := roundTrip(t, c2, "y")[:2]; got != first2 {
			t.Fatalf("c2 moved %q -> %q", first2, got)
		}
	}
	if h.l.Active() != 2 {
		t.Fatalf("active = %d", h.l.Active())
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	a, _ := echo(t, "A:")
	cfg := baseCfg()
	cfg.UDP.SessionIdleTimeout = config.Duration(100 * time.Millisecond)
	h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	c := h.client(t)
	roundTrip(t, c, "x")
	eventually(t, "session expiry", func() bool { return h.l.Active() == 0 })
	// A new datagram opens a fresh session.
	roundTrip(t, c, "again")
	if h.rec.open.Load() != 2 {
		t.Fatalf("open events = %d, want 2", h.rec.open.Load())
	}
}

func TestLimits(t *testing.T) {
	a, _ := echo(t, "A:")
	t.Run("max sessions", func(t *testing.T) {
		cfg := baseCfg()
		cfg.UDP.MaxSessions = 1
		h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
		roundTrip(t, h.client(t), "x")
		c2 := h.client(t)
		_, _ = c2.Write([]byte("x"))
		eventually(t, "limit_listener rejection", func() bool { return h.rec.rej("limit_listener") > 0 })
		if h.l.Active() != 1 {
			t.Fatalf("active = %d", h.l.Active())
		}
	})
	t.Run("per source", func(t *testing.T) {
		cfg := baseCfg()
		cfg.UDP.MaxSessionsPerSource = 1 // all loopback clients share one source IP
		h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
		roundTrip(t, h.client(t), "x")
		_, _ = h.client(t).Write([]byte("x"))
		eventually(t, "limit_source rejection", func() bool { return h.rec.rej("limit_source") > 0 })
	})
	t.Run("global", func(t *testing.T) {
		g := limits.NewGate(1)
		h := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{newBackend(a)}}, g)
		roundTrip(t, h.client(t), "x")
		_, _ = h.client(t).Write([]byte("x"))
		eventually(t, "limit_global rejection", func() bool { return h.rec.rej("limit_global") > 0 })
	})
	t.Run("no backend", func(t *testing.T) {
		h := start(t, baseCfg(), &fakePool{}, nil)
		_, _ = h.client(t).Write([]byte("x"))
		eventually(t, "no_backend rejection", func() bool { return h.rec.rej("no_backend") > 0 })
		if h.l.Active() != 0 {
			t.Fatal("session leaked")
		}
	})
}

func TestACL(t *testing.T) {
	a, _ := echo(t, "A:")
	cfg := baseCfg()
	cfg.Access.Deny = []string{"127.0.0.0/8"}
	h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	_, _ = h.client(t).Write([]byte("x"))
	eventually(t, "acl rejection", func() bool { return h.rec.rej("acl") > 0 })
	if h.l.Active() != 0 || h.rec.open.Load() != 0 {
		t.Fatal("denied client got a session")
	}
}

func TestRefusedBackendEjects(t *testing.T) {
	// Reserve a UDP port, then close it so the kernel answers with ICMP port unreachable.
	tmp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	dead := tmp.LocalAddr().(*net.UDPAddr).AddrPort()
	_ = tmp.Close()

	pool := &fakePool{bs: []*backend.Backend{newBackend(dead)}}
	h := start(t, baseCfg(), pool, nil)
	_, _ = h.client(t).Write([]byte("x"))
	deadline := time.Now().Add(2 * time.Second)
	for pool.ejected() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pool.ejected() == 0 {
		t.Skipf("%s did not surface ECONNREFUSED on a connected UDP socket", runtime.GOOS)
	}
	eventually(t, "session end", func() bool { return h.l.Active() == 0 })
	recs := h.log.all()
	if len(recs) != 1 || recs[0].Result != "backend_reset" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestDrain(t *testing.T) {
	a, _ := echo(t, "A:")
	cfg := baseCfg()
	cfg.UDP.SessionIdleTimeout = config.Duration(200 * time.Millisecond)
	h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	c := h.client(t)
	roundTrip(t, c, "x")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained := make(chan int, 1)
	go func() { drained <- h.l.Drain(ctx) }()
	eventually(t, "draining flag", func() bool { return h.l.draining.Load() })
	// Existing session keeps forwarding; a new client is refused.
	if got := roundTrip(t, c, "y"); got != "A:y" {
		t.Fatalf("got %q", got)
	}
	_, _ = h.client(t).Write([]byte("z"))
	eventually(t, "draining rejection", func() bool { return h.rec.rej("draining") > 0 })
	select {
	case forced := <-drained:
		if forced != 0 {
			t.Fatalf("forced = %d, want 0 (sessions idle out)", forced)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Drain did not return")
	}
	<-h.done

	// Context expiry forces remaining sessions closed.
	h2 := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	roundTrip(t, h2.client(t), "x")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if forced := h2.l.Drain(ctx2); forced != 1 {
		t.Fatalf("forced = %d, want 1", forced)
	}
}

func TestAccessLogOnExpiry(t *testing.T) {
	a, _ := echo(t, "A:")
	cfg := baseCfg()
	cfg.UDP.SessionIdleTimeout = config.Duration(100 * time.Millisecond)
	h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	c := h.client(t)
	roundTrip(t, c, "abc")
	roundTrip(t, c, "defg")
	eventually(t, "access log record", func() bool { return len(h.log.all()) == 1 })
	r := h.log.all()[0]
	if r.Protocol != "udp" || r.Result != "ok" || r.Listener != "dns" || r.Backend != a.String() {
		t.Fatalf("record = %+v", r)
	}
	if r.BytesIn != 7 || r.BytesOut != 11 || r.Duration <= 0 || r.Client == "" {
		t.Fatalf("counters = in %d out %d dur %v client %q", r.BytesIn, r.BytesOut, r.Duration, r.Client)
	}
}

func TestExactlyOneCloseEventPerSession(t *testing.T) {
	a, _ := echo(t, "A:")
	cfg := baseCfg()
	cfg.UDP.SessionIdleTimeout = config.Duration(80 * time.Millisecond)
	h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	for i := 0; i < 20; i++ {
		roundTrip(t, h.client(t), "x")
	}
	eventually(t, "all sessions expired", func() bool { return h.l.Active() == 0 })
	time.Sleep(200 * time.Millisecond)
	if o, c := h.rec.open.Load(), h.rec.closing.Load(); o != 20 || c != 20 {
		t.Fatalf("open=%d closing=%d, want 20/20", o, c)
	}
	if n := len(h.log.all()); n != 20 {
		t.Fatalf("access records = %d, want 20", n)
	}
}

func TestUpdate(t *testing.T) {
	a, _ := echo(t, "A:")
	b, _ := echo(t, "B:")
	pa := &fakePool{bs: []*backend.Backend{newBackend(a)}}
	h := start(t, baseCfg(), pa, nil)
	if got := roundTrip(t, h.client(t), "x"); got != "A:x" {
		t.Fatalf("got %q", got)
	}
	bad := baseCfg()
	bad.ListenerAddress = "127.0.0.1:1"
	if err := h.l.Update(bad, map[string]Pool{"dns": pa}); !errors.Is(err, ErrAddressChanged) {
		t.Fatalf("err = %v", err)
	}
	cfg := baseCfg()
	cfg.ListenerAddress = h.l.Address()
	if err := h.l.Update(cfg, map[string]Pool{"dns": &fakePool{bs: []*backend.Backend{newBackend(b)}}}); err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, h.client(t), "x"); got != "B:x" {
		t.Fatalf("after update got %q", got)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	a, stop := echo(t, "A:")
	cfg := baseCfg()
	h := start(t, cfg, &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	for i := 0; i < 10; i++ {
		roundTrip(t, h.client(t), "x")
	}
	if h.l.Active() != 10 {
		t.Fatalf("active = %d", h.l.Active())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	h.l.Drain(ctx)
	<-h.done
	stop()

	eventually(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= base })
}

func TestCapSkipUsesNextCandidate(t *testing.T) {
	a, _ := echo(t, "A:")
	b, _ := echo(t, "B:")
	full := backend.New(backend.Endpoint{Address: a, Weight: 1}, 1)
	if !full.TryAcquire() { // pre-fill A's only slot
		t.Fatal("setup acquire")
	}
	h := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{full, newBackend(b)}, all: true}, nil)
	if got := roundTrip(t, h.client(t), "x"); got != "B:x" {
		t.Fatalf("reply = %q, want B:x", got)
	}
	if h.rec.rej("no_backend") != 0 {
		t.Fatal("unexpected no_backend rejection")
	}
}

func TestAllCandidatesAtCapRejects(t *testing.T) {
	a, _ := echo(t, "A:")
	full := backend.New(backend.Endpoint{Address: a, Weight: 1}, 1)
	full.TryAcquire()
	h := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{full}, all: true}, nil)
	_, _ = h.client(t).Write([]byte("x"))
	eventually(t, "no_backend rejection", func() bool { return h.rec.rej("no_backend") > 0 })
}

func TestDrainForcedMetric(t *testing.T) {
	a, _ := echo(t, "A:")
	h := start(t, baseCfg(), &fakePool{bs: []*backend.Backend{newBackend(a)}}, nil)
	roundTrip(t, h.client(t), "x")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if forced := h.l.Drain(ctx); forced != 1 {
		t.Fatalf("forced = %d, want 1", forced)
	}
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if len(h.rec.drained) != 1 || h.rec.drained[0] != 1 {
		t.Fatalf("DrainForced calls = %v, want [1]", h.rec.drained)
	}
}
