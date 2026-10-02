package loadbalancer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/backend"
	"github.com/cloudresty/nautiluslb/config"
)

func testConfig() config.Configuration {
	return config.Configuration{
		Name:            "test-lb",
		ListenerAddress: "127.0.0.1:0",
		RequestTimeout:  5,
		BackendPortName: "http",
	}
}

func newTestLB(t *testing.T, backends ...*backend.BackendServer) *LoadBalancer {
	t.Helper()
	lb := NewLoadBalancer(testConfig(), 5*time.Second)
	lb.SetBackendServers(backends)
	return lb
}

// serve runs lb on a loopback listener and returns its address.
func serve(t *testing.T, lb *LoadBalancer) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	served := make(chan struct{})
	go func() {
		defer close(served)
		lb.Serve(listener)
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		<-served
	})

	return listener.Addr().String()
}

func backendAt(t *testing.T, addr string) *backend.BackendServer {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return backend.New(port, host, port, "http")
}

// refusingAddr returns a loopback address with nothing listening on it, so a
// dial is refused.
func refusingAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

// echoServer echoes everything it reads, and half-closes its side when the
// client half-closes.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
				_ = conn.(*net.TCPConn).CloseWrite()
			}()
		}
	}()

	return listener.Addr().String()
}

// waitClosed asserts the server side closes conn within d.
func waitClosed(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.ReadAll(conn)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("client connection still open after %v", d)
	}
}

func roundTrip(t *testing.T, addr string, payload []byte) []byte {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial lb: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// The defect this file exists for: a refused dial used to fall through to the
// copy with a nil connection, and the resulting panic in a goroutine killed
// the process. If this regresses, the test binary itself dies.
func TestRefusedBackendClosesClientWithoutCrashing(t *testing.T) {

	lb := newTestLB(t, backendAt(t, refusingAddr(t)))
	addr := serve(t, lb)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial lb: %v", err)
	}
	defer func() { _ = conn.Close() }()

	waitClosed(t, conn, 5*time.Second)

	// The process is still serving: a second connection is accepted.
	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("load balancer stopped accepting after a failed dial: %v", err)
	}
	_ = conn2.Close()
}

func TestRefusedBackendFailsOverToNextBackend(t *testing.T) {

	refused := backendAt(t, refusingAddr(t))
	good := backendAt(t, echoServer(t))

	lb := newTestLB(t, refused, good)
	// Make the refusing backend the first choice.
	lb.nextServer = len(lb.backendServers) - 1

	addr := serve(t, lb)

	got := roundTrip(t, addr, []byte("hello"))
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}

	if refused.IsHealthy() {
		t.Error("refusing backend should have been ejected after a failed dial")
	}
}

// blackholeDial behaves like a dial to a host that drops SYNs: it never
// connects and returns only when its context ends.
func blackholeDial(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBlackholedBackendIsBoundedByDialTimeout(t *testing.T) {

	lb := newTestLB(t, backend.New(1, "192.0.2.1", 80, "http"))
	lb.dialTimeout = 100 * time.Millisecond
	lb.dial = blackholeDial

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		lb.HandleConnection(server)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConnection did not give up on a blackholed backend")
	}
}

func TestBlackholedBackendFailsOverWithinRetryBudget(t *testing.T) {

	good := backendAt(t, echoServer(t))
	hole := backend.New(1, "192.0.2.1", 80, "http")

	lb := newTestLB(t, hole, good)
	lb.nextServer = len(lb.backendServers) - 1
	lb.dialTimeout = 100 * time.Millisecond

	real := lb.dial
	lb.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == hole.Address() {
			return blackholeDial(ctx, network, address)
		}
		return real(ctx, network, address)
	}

	addr := serve(t, lb)

	start := time.Now()
	got := roundTrip(t, addr, []byte("ping"))
	if string(got) != "ping" {
		t.Fatalf("got %q, want %q", got, "ping")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("failover took %v", elapsed)
	}
}

func TestEveryBackendFailingStaysWithinRetryBudget(t *testing.T) {

	var backends []*backend.BackendServer
	for i := range 5 {
		backends = append(backends, backend.New(i, "192.0.2.1", 1000+i, "http"))
	}

	lb := newTestLB(t, backends...)
	lb.dialTimeout = 50 * time.Millisecond

	var mu sync.Mutex
	attempts := 0
	lb.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return blackholeDial(ctx, network, address)
	}

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	lb.HandleConnection(server)

	if attempts != maxDialAttempts {
		t.Errorf("dial attempts = %d, want %d", attempts, maxDialAttempts)
	}
}

// panicReadConn and panicWriteConn panic, standing in for any bug reached
// while proxying. Reading the backend happens on the handler's goroutine and
// writing to it on a second one; a panic on either must be contained.
type panicReadConn struct{ net.Conn }

func (panicReadConn) Read([]byte) (int, error) { panic("boom on read") }

type panicWriteConn struct{ net.Conn }

func (panicWriteConn) Write([]byte) (int, error) { panic("boom on write") }

func TestPanicWhileProxyingIsContainedToTheConnection(t *testing.T) {

	wrappers := map[string]func(net.Conn) net.Conn{
		"backend read":  func(c net.Conn) net.Conn { return panicReadConn{c} },
		"backend write": func(c net.Conn) net.Conn { return panicWriteConn{c} },
	}

	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {

			lb := newTestLB(t, backend.New(1, "192.0.2.1", 80, "http"))
			lb.dial = func(context.Context, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				go func() { _, _ = io.Copy(io.Discard, b) }()
				return wrap(a), nil
			}

			client, server := net.Pipe()
			defer func() { _ = client.Close() }()

			done := make(chan struct{})
			go func() {
				defer close(done)
				lb.HandleConnection(server)
			}()

			// Give the client-to-backend direction something to write.
			go func() { _, _ = client.Write([]byte("payload")) }()

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("HandleConnection did not return after a panic")
			}
		})
	}
}

// A panic outside the copy loops (here, in the dial) is caught by the
// handler's own recover.
func TestPanicBeforeProxyingIsContained(t *testing.T) {

	lb := newTestLB(t, backend.New(1, "192.0.2.1", 80, "http"))
	lb.dial = func(context.Context, string, string) (net.Conn, error) { panic("boom in dial") }

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		lb.HandleConnection(server)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConnection did not return after a panic")
	}

	waitClosed(t, client, 3*time.Second)
}

func TestHalfCloseIsForwardedAndBothSidesFinish(t *testing.T) {

	lb := newTestLB(t, backendAt(t, echoServer(t)))
	addr := serve(t, lb)

	payload := bytes.Repeat([]byte("x"), 1<<20)
	got := roundTrip(t, addr, payload)
	if !bytes.Equal(got, payload) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(payload))
	}
}

// A backend that dies mid-connection must close the client connection too,
// rather than leaving it, and a goroutine, hanging.
func TestBackendResetClosesClient(t *testing.T) {

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close() // RST
	}()

	lb := newTestLB(t, backendAt(t, listener.Addr().String()))
	addr := serve(t, lb)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial lb: %v", err)
	}
	defer func() { _ = conn.Close() }()

	waitClosed(t, conn, 5*time.Second)
}

func TestNoGoroutineLeakAfterConnections(t *testing.T) {

	lb := newTestLB(t, backendAt(t, echoServer(t)), backendAt(t, refusingAddr(t)))
	addr := serve(t, lb)

	// Warm up so lazily started runtime goroutines are not counted.
	roundTrip(t, addr, []byte("warm"))
	before := runtime.NumGoroutine()

	for range 20 {
		conn, err := net.Dial("tcp", addr)
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

func TestDialTimeoutFor(t *testing.T) {
	tests := []struct {
		in, want time.Duration
	}{
		{0, defaultDialTimeout},
		{-1, defaultDialTimeout},
		{5 * time.Second, 5 * time.Second},
		{300 * time.Second, maxDialTimeout},
	}
	for _, tt := range tests {
		if got := dialTimeoutFor(tt.in); got != tt.want {
			t.Errorf("dialTimeoutFor(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNextBackendsRoundRobinSkipsUnhealthy(t *testing.T) {

	a := backend.New(1, "10.0.0.1", 80, "http")
	b := backend.New(2, "10.0.0.2", 80, "http")
	c := backend.New(3, "10.0.0.3", 80, "http")
	other := backend.New(4, "10.0.0.4", 81, "https")
	b.SetHealthy(false)

	lb := newTestLB(t, a, b, c, other)

	seen := map[int]int{}
	for range 10 {
		server := lb.getNextBackend()
		if server == nil {
			t.Fatal("expected a backend")
		}
		seen[server.ID]++
	}

	if seen[2] != 0 {
		t.Errorf("unhealthy backend selected %d times", seen[2])
	}
	if seen[4] != 0 {
		t.Errorf("backend for another port name selected %d times", seen[4])
	}
	if seen[1] != 5 || seen[3] != 5 {
		t.Errorf("round robin uneven: %v", seen)
	}

	picked := lb.nextBackends(maxDialAttempts)
	if len(picked) != 2 || picked[0] == picked[1] {
		t.Errorf("nextBackends should return distinct healthy backends, got %d", len(picked))
	}
}

func TestNextBackendsFallsBackWhenNoneHealthy(t *testing.T) {

	a := backend.New(1, "10.0.0.1", 80, "http")
	b := backend.New(2, "10.0.0.2", 80, "http")
	a.SetHealthy(false)
	b.SetHealthy(false)

	lb := newTestLB(t, a, b)

	if got := lb.nextBackends(maxDialAttempts); len(got) != 2 {
		t.Errorf("with every backend unhealthy, expected both as a fallback, got %d", len(got))
	}
}

func TestNextBackendsEmpty(t *testing.T) {
	lb := newTestLB(t)
	if got := lb.getNextBackend(); got != nil {
		t.Error("expected nil with no backends")
	}
}

// Rediscovering the same backends must keep their objects, and so their
// health state and running health check.
func TestSetBackendServersPreservesKnownBackends(t *testing.T) {

	lb := newTestLB(t, backend.New(1, "10.0.0.1", 80, "http"))
	original := lb.GetBackendServers()[0]
	original.SetHealthy(false)

	lb.SetBackendServers([]*backend.BackendServer{
		backend.New(9, "10.0.0.1", 80, "http"),
		backend.New(10, "10.0.0.2", 80, "http"),
	})

	servers := lb.GetBackendServers()
	if len(servers) != 2 {
		t.Fatalf("got %d backends, want 2", len(servers))
	}
	if servers[0] != original {
		t.Error("known backend was replaced by a fresh object")
	}
	if servers[0].IsHealthy() {
		t.Error("known unhealthy backend was reset to healthy by rediscovery")
	}
}

func TestHealthChecksFollowBackendsAndStop(t *testing.T) {

	lb := newTestLB(t, backend.New(1, "127.0.0.1", 1, "http"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if !lb.begin(listener) {
		t.Fatal("begin refused")
	}

	count := func() int {
		lb.mu.RLock()
		defer lb.mu.RUnlock()
		return len(lb.healthChecks)
	}

	if got := count(); got != 1 {
		t.Fatalf("health checks after Start = %d, want 1", got)
	}

	lb.SetBackendServers([]*backend.BackendServer{
		backend.New(2, "127.0.0.1", 2, "http"),
		backend.New(3, "127.0.0.1", 3, "http"),
	})
	if got := count(); got != 2 {
		t.Fatalf("health checks after rediscovery = %d, want 2", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		lb.Stop()
		lb.Stop() // idempotent
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not wait for health checks to exit")
	}

	if got := count(); got != 0 {
		t.Errorf("health checks after Stop = %d, want 0", got)
	}
}

func TestStopBeforeStart(t *testing.T) {

	lb := newTestLB(t)
	lb.Stop()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	if lb.begin(listener) {
		t.Error("begin after Stop should refuse")
	}
}

// Run under -race: discovery, health checks and connection handlers touch
// the same backends concurrently.
func TestConcurrentDiscoveryHealthAndSelection(t *testing.T) {

	lb := newTestLB(t, backend.New(1, "10.0.0.1", 80, "http"))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			lb.SetBackendServers([]*backend.BackendServer{
				backend.New(1, "10.0.0.1", 80, "http"),
				backend.New(2, "10.0.0.2", 80+i%2, "http"),
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			for _, server := range lb.GetBackendServers() {
				server.SetHealthy(i%2 == 0)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if server := lb.getNextBackend(); server != nil {
				server.Acquire()
				server.Release()
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestListenReportsBindFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()

	cfg := testConfig()
	cfg.ListenerAddress = taken.Addr().String()
	lb := NewLoadBalancer(cfg, time.Second)

	if err := lb.Listen(); err == nil {
		t.Fatal("Listen succeeded on a port in use")
	}
	if lb.GetListener() != nil {
		t.Fatal("listener recorded after a failed bind")
	}
}
