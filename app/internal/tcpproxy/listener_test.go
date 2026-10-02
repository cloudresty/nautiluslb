package tcpproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
)

// The defect these tests exist for: a refused dial used to fall through to
// the copy with a nil connection, and the panic killed the process.
func TestRefusedBackendClosesClientWithoutCrashing(t *testing.T) {
	p := newFakePool("test", nb(t, tpRefusing(t)))
	h := startTP(t, tpConfig(), poolsOf(p), nil)

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tpWaitClosed(t, conn, 5*time.Second)

	conn2, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("listener stopped accepting after a failed dial: %v", err)
	}
	_ = conn2.Close()

	recs := h.log.waitN(t, 1)
	if recs[0].Result != accesslog.ResultDialFailed {
		t.Errorf("result = %q, want dial_failed", recs[0].Result)
	}
}

func TestRefusedBackendFailsOverToNextBackend(t *testing.T) {
	refused := nb(t, tpRefusing(t))
	good := nb(t, tpEcho(t))
	p := newFakePool("test", refused, good) // refused is the first choice
	h := startTP(t, tpConfig(), poolsOf(p), nil)

	if got := tpRoundTrip(t, h.addr, []byte("hello")); string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
	if refused.IsHealthy() {
		t.Error("refused backend should have been ejected")
	}
	if p.ejections() != 1 {
		t.Errorf("ejections = %d, want 1", p.ejections())
	}
}

func TestBlackholedBackendIsBoundedByDialTimeout(t *testing.T) {
	cfg := tpConfig()
	cfg.DialTimeout = config.Duration(100 * time.Millisecond)
	p := newFakePool("test", nb(t, "192.0.2.1:80"))
	h := newTP(t, cfg, poolsOf(p), func(o *Options) { o.Dial = tpBlackhole })

	client, done := runHandle(h.l)
	defer func() { _ = client.Close() }()
	waitDone(t, done, 3*time.Second, "handle on a blackholed backend")
	tpWaitClosed(t, client, time.Second)
}

func TestBlackholedBackendFailsOverWithinRetryBudget(t *testing.T) {
	good := nb(t, tpEcho(t))
	hole := nb(t, "192.0.2.1:80")
	cfg := tpConfig()
	cfg.DialTimeout = config.Duration(100 * time.Millisecond)
	p := newFakePool("test", hole, good)

	var real net.Dialer
	h := startTP(t, cfg, poolsOf(p), func(o *Options) {
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == hole.Address() {
				return tpBlackhole(ctx, network, addr)
			}
			return real.DialContext(ctx, network, addr)
		}
	})

	start := time.Now()
	if got := tpRoundTrip(t, h.addr, []byte("ping")); string(got) != "ping" {
		t.Fatalf("got %q, want ping", got)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("failover took %v", el)
	}
}

func TestEveryBackendFailingStaysWithinRetryBudget(t *testing.T) {
	var bs []*backend.Backend
	for i := range 5 {
		bs = append(bs, nb(t, "192.0.2.1:"+string(rune('1'+i))+"000"))
	}
	cfg := tpConfig()
	cfg.DialTimeout = config.Duration(50 * time.Millisecond)

	var mu sync.Mutex
	attempts := 0
	h := newTP(t, cfg, poolsOf(newFakePool("test", bs...)), func(o *Options) {
		o.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
			mu.Lock()
			attempts++
			mu.Unlock()
			return tpBlackhole(ctx, n, a)
		}
	})

	client, done := runHandle(h.l)
	defer func() { _ = client.Close() }()
	waitDone(t, done, 3*time.Second, "handle")

	if attempts != dialBudget {
		t.Errorf("dial attempts = %d, want %d", attempts, dialBudget)
	}
	for _, b := range bs {
		if b.ActiveConnections() != 0 {
			t.Errorf("backend %s still holds %d slots", b.Address(), b.ActiveConnections())
		}
	}
}

func TestPanicWhileProxyingIsContainedToTheConnection(t *testing.T) {
	wrappers := map[string]func(net.Conn) net.Conn{
		"backend read":  func(c net.Conn) net.Conn { return tpPanicRead{c} },
		"backend write": func(c net.Conn) net.Conn { return tpPanicWrite{c} },
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			p := newFakePool("test", nb(t, "192.0.2.1:80"))
			h := newTP(t, tpConfig(), poolsOf(p), func(o *Options) {
				o.Dial = func(context.Context, string, string) (net.Conn, error) {
					a, b := net.Pipe()
					go func() { _, _ = io.Copy(io.Discard, b) }()
					return wrap(a), nil
				}
			})

			client, done := runHandle(h.l)
			defer func() { _ = client.Close() }()
			go func() { _, _ = client.Write([]byte("payload")) }()
			waitDone(t, done, 3*time.Second, "handle after a panic")
		})
	}
}

func TestPanicBeforeProxyingIsContained(t *testing.T) {
	p := newFakePool("test", nb(t, "192.0.2.1:80"))
	h := newTP(t, tpConfig(), poolsOf(p), func(o *Options) {
		o.Dial = func(context.Context, string, string) (net.Conn, error) { panic("boom in dial") }
	})

	client, done := runHandle(h.l)
	defer func() { _ = client.Close() }()
	waitDone(t, done, 3*time.Second, "handle after a panic")
	tpWaitClosed(t, client, 3*time.Second)

	recs := h.log.waitN(t, 1)
	if recs[0].Result != accesslog.ResultPanic {
		t.Errorf("result = %q, want panic", recs[0].Result)
	}
	if h.l.Active() != 0 || h.l.gate.Active() != 0 {
		t.Errorf("slots leaked after panic: active=%d gate=%d", h.l.Active(), h.l.gate.Active())
	}
	if b := p.bs[0]; b.ActiveConnections() != 0 {
		t.Errorf("backend slot leaked: %d", b.ActiveConnections())
	}
}

func TestHalfCloseIsForwardedAndBothSidesFinish(t *testing.T) {
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
	payload := bytes.Repeat([]byte("x"), 1<<20)
	if got := tpRoundTrip(t, h.addr, payload); !bytes.Equal(got, payload) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(payload))
	}
	recs := h.log.waitN(t, 1)
	if recs[0].Result != accesslog.ResultOK || recs[0].BytesIn != int64(len(payload)) {
		t.Errorf("record = %+v", recs[0])
	}
}

func TestBackendResetClosesClient(t *testing.T) {
	addr := tpServe(t, func(c net.Conn) { _ = c.(*net.TCPConn).SetLinger(0) }) // close = RST
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, addr))), nil)

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tpWaitClosed(t, conn, 5*time.Second)
	h.log.waitN(t, 1)
}

func TestNoGoroutineLeakAfterConnections(t *testing.T) {
	p := newFakePool("test", nb(t, tpEcho(t)), nb(t, tpRefusing(t)))
	h := startTP(t, tpConfig(), poolsOf(p), nil)

	tpRoundTrip(t, h.addr, []byte("warm"))
	before := runtime.NumGoroutine()

	for range 20 {
		conn, err := net.Dial("tcp", h.addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_, _ = conn.Write([]byte("x"))
		_ = conn.(*net.TCPConn).CloseWrite()
		_, _ = io.ReadAll(conn)
		_ = conn.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines: before %d, after %d\n%s", before, runtime.NumGoroutine(), buf[:n])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every backend unhealthy: the pool fails open and the listener dials anyway,
// it does not filter by health itself.
func TestPickFailsOpen(t *testing.T) {
	b := nb(t, tpEcho(t))
	b.SetHealthy(false, backend.CauseProbe)
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", b)), nil)
	if got := tpRoundTrip(t, h.addr, []byte("fail-open")); string(got) != "fail-open" {
		t.Fatalf("got %q", got)
	}
}

func TestNoBackendClosesClient(t *testing.T) {
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test")), nil)
	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	tpWaitClosed(t, conn, 3*time.Second)
	if recs := h.log.waitN(t, 1); recs[0].Result != accesslog.ResultNoBackend {
		t.Errorf("result = %q", recs[0].Result)
	}
}

func TestBackendAtCapIsSkipped(t *testing.T) {
	capped := backend.New(backend.Endpoint{Address: nb(t, tpEcho(t)).Endpoint().Address, Weight: 1}, 1)
	if !capped.TryAcquire() { // fill it
		t.Fatal("could not fill backend")
	}
	good := nb(t, tpEcho(t))
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", capped, good)), nil)
	if got := tpRoundTrip(t, h.addr, []byte("cap")); string(got) != "cap" {
		t.Fatalf("got %q", got)
	}
	if capped.ActiveConnections() != 1 {
		t.Errorf("capped backend active = %d, want 1 (the pre-filled slot)", capped.ActiveConnections())
	}
}

// errListener fails Accept n times, then reports a closed listener.
type errListener struct {
	net.Listener
	mu    sync.Mutex
	times []time.Time
	n     int
}

func (e *errListener) Close() error { return nil }

func (e *errListener) Accept() (net.Conn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.times = append(e.times, time.Now())
	if len(e.times) <= e.n {
		return nil, errors.New("accept: too many open files")
	}
	return nil, net.ErrClosed
}

func TestAcceptBackoff(t *testing.T) {
	l, err := New(Options{Config: tpConfig()})
	if err != nil {
		t.Fatal(err)
	}
	el := &errListener{n: 4}
	start := time.Now()
	done := make(chan struct{})
	go func() { l.serveOn(el); close(done) }()
	waitDone(t, done, 5*time.Second, "serveOn")

	// Backoff doubles from 5ms: 5+10+20+40 = 75ms before the fifth accept.
	if el := time.Since(start); el < 70*time.Millisecond {
		t.Errorf("accept errors were retried without backoff (%v)", el)
	}
	if len(el.times) != 5 {
		t.Errorf("accept calls = %d, want 5", len(el.times))
	}
}

func TestAcceptBackoffStopsOnDrain(t *testing.T) {
	l, err := New(Options{Config: tpConfig()})
	if err != nil {
		t.Fatal(err)
	}
	el := &errListener{n: 1 << 30}
	l.mu.Lock()
	l.ln, l.started = el, true
	l.mu.Unlock()
	go l.serveOn(el)

	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() { l.Drain(ctx); close(finished) }()
	waitDone(t, finished, 3*time.Second, "Drain with a failing listener")
}

func TestListenReportsBindFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	cfg := tpConfig()
	cfg.ListenerAddress = taken.Addr().String()
	l, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Listen(); err == nil {
		t.Fatal("Listen succeeded on a port in use")
	}
	if l.Address() != cfg.ListenerAddress {
		t.Errorf("Address() = %q after failed bind", l.Address())
	}
}

// Metrics model: accepted == closed (reached pipe.Run) + rejected after
// accept. A failed dial is a rejection only, never also a close.
func TestDialFailedIsRejectedNotClosed(t *testing.T) {
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, tpRefusing(t)))), nil)
	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tpWaitClosed(t, conn, 5*time.Second)
	h.log.waitN(t, 1)

	eventually(t, 3*time.Second, func() bool { return h.rec.get(func(r *fakeRec) int { return r.rejected["dial_failed"] }) == 1 }, "rejected{dial_failed}")
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	closed := 0
	for _, n := range h.rec.closed {
		closed += n
	}
	if h.rec.accepted != 1 || closed != 0 || h.rec.active != 0 {
		t.Errorf("accepted=%d closed=%d active=%d, want 1/0/0", h.rec.accepted, closed, h.rec.active)
	}
}
