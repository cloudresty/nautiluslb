package tcpproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// fakePool is a Pool with round-robin order, health-first ordering and
// fail-open, like the real one, plus a record of ejections.
type fakePool struct {
	key string

	mu      sync.Mutex
	bs      []*backend.Backend
	next    int
	ejected []*backend.Backend
	picks   int
}

func newFakePool(key string, bs ...*backend.Backend) *fakePool {
	return &fakePool{key: key, bs: bs}
}

func (p *fakePool) Key() string { return p.key }

func (p *fakePool) Pick(_ balancer.Key, n int) []*backend.Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.picks++
	var healthy []*backend.Backend
	for _, b := range p.bs {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		}
	}
	set := healthy
	if len(set) == 0 { // fail open
		set = p.bs
	}
	if len(set) == 0 {
		return nil
	}
	start := p.next % len(set)
	p.next++
	out := make([]*backend.Backend, 0, min(n, len(set)))
	for i := 0; i < len(set) && len(out) < n; i++ {
		out = append(out, set[(start+i)%len(set)])
	}
	return out
}

func (p *fakePool) Eject(b *backend.Backend) {
	p.mu.Lock()
	p.ejected = append(p.ejected, b)
	p.mu.Unlock()
	b.SetHealthy(false, backend.CausePassive)
}

func (p *fakePool) ejections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ejected)
}

// fakeRec records the metrics the data path emits.
type fakeRec struct {
	metrics.Recorder

	mu       sync.Mutex
	accepted int
	rejected map[string]int
	active   int
	closed   map[string]int
	dials    map[string]int
	bActive  int
	modes    map[string]int
	bytesIn  int64
	bytesOut int64
}

func newFakeRec() *fakeRec {
	return &fakeRec{
		Recorder: metrics.NewNop(),
		rejected: map[string]int{}, closed: map[string]int{}, dials: map[string]int{}, modes: map[string]int{},
	}
}

func (r *fakeRec) ConnAccepted(string) { r.mu.Lock(); r.accepted++; r.mu.Unlock() }
func (r *fakeRec) ConnRejected(_, reason string) {
	r.mu.Lock()
	r.rejected[reason]++
	r.mu.Unlock()
}
func (r *fakeRec) ConnActive(_ string, d int) { r.mu.Lock(); r.active += d; r.mu.Unlock() }
func (r *fakeRec) ConnClosed(_, result string, _ time.Duration, in, out int64) {
	r.mu.Lock()
	r.closed[result]++
	r.bytesIn += in
	r.bytesOut += out
	r.mu.Unlock()
}
func (r *fakeRec) BackendDial(_, _, _, result string, _ time.Duration) {
	r.mu.Lock()
	r.dials[result]++
	r.mu.Unlock()
}
func (r *fakeRec) BackendActive(_, _, _ string, d int) { r.mu.Lock(); r.bActive += d; r.mu.Unlock() }
func (r *fakeRec) PipeMode(m string)                   { r.mu.Lock(); r.modes[m]++; r.mu.Unlock() }

func (r *fakeRec) get(f func(*fakeRec) int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return f(r)
}

// fakeLog collects access-log records.
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

func (f *fakeLog) waitN(t *testing.T, n int) []accesslog.Record {
	t.Helper()
	var got []accesslog.Record
	eventually(t, 5*time.Second, func() bool { got = f.all(); return len(got) >= n }, "access-log records")
	return got
}

func eventually(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func tpConfig() config.Configuration {
	return config.Configuration{
		Name:            "test",
		Protocol:        config.ProtocolTCP,
		ListenerAddress: "127.0.0.1:0",
		BackendPortName: "http",
	}
}

func nb(t *testing.T, addr string) *backend.Backend {
	t.Helper()
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		t.Fatalf("parse %q: %v", addr, err)
	}
	return backend.New(backend.Endpoint{Address: ap, Weight: 1}, 0)
}

type harness struct {
	l    *Listener
	addr string
	rec  *fakeRec
	log  *fakeLog
}

// startTP builds a Listener on a loopback port, serves it, and drains it at
// cleanup. mod may adjust the options before New.
func startTP(t *testing.T, cfg config.Configuration, pools map[string]Pool, mod func(*Options)) *harness {
	t.Helper()
	h := newTP(t, cfg, pools, mod)
	if err := h.l.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go h.l.Serve()
	h.addr = h.l.Address()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.l.Drain(ctx)
	})
	return h
}

// newTP builds a Listener without binding, for tests that call handle on
// net.Pipe connections.
func newTP(t *testing.T, cfg config.Configuration, pools map[string]Pool, mod func(*Options)) *harness {
	t.Helper()
	h := &harness{rec: newFakeRec(), log: &fakeLog{}}
	opts := Options{Config: cfg, Pools: pools, Recorder: h.rec, AccessLog: h.log}
	if mod != nil {
		mod(&opts)
	}
	l, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.l = l
	return h
}

func poolsOf(p *fakePool) map[string]Pool { return map[string]Pool{p.key: p} }

func tpRefusing(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// tpEcho echoes everything it reads and half-closes when the client does.
func tpEcho(t *testing.T) string {
	t.Helper()
	return tpServe(t, func(c net.Conn) {
		_, _ = io.Copy(c, c)
		_ = c.(*net.TCPConn).CloseWrite()
	})
}

// tpServe runs fn for every accepted connection and closes it afterwards.
func tpServe(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				fn(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func tpWaitClosed(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.ReadAll(conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("connection still open after %v", d)
	}
}

func tpRoundTrip(t *testing.T, addr string, payload []byte) []byte {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.(*net.TCPConn).CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// tpBlackhole behaves like a dial to a host that drops SYNs.
func tpBlackhole(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type tpPanicRead struct{ net.Conn }

func (tpPanicRead) Read([]byte) (int, error) { panic("boom on read") }

type tpPanicWrite struct{ net.Conn }

func (tpPanicWrite) Write([]byte) (int, error) { panic("boom on write") }

// runHandle runs l.handle on one end of a net.Pipe and returns the other end
// and a channel closed when handle returns.
func runHandle(l *Listener) (net.Conn, <-chan struct{}) {
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.handle(server)
	}()
	return client, done
}

func waitDone(t *testing.T, done <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

func nbAddr(ap netip.AddrPort) *backend.Backend {
	return backend.New(backend.Endpoint{Address: ap, Weight: 1}, 0)
}
