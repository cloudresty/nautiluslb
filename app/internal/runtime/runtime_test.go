package runtime

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStartupBindAllOrFail(t *testing.T) {
	a := freeAddr(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	cfg := mustCfg(t, doc("1s", tcpCfg("a", a, ""), tcpCfg("b", taken.Addr().String(), "")))
	rt, err := New(Options{Config: cfg, Client: fake.NewSimpleClientset()})
	if err == nil {
		rt.Shutdown(context.Background())
		t.Fatal("New succeeded with a taken port")
	}
	ln, err := net.Listen("tcp", a)
	if err != nil {
		t.Fatalf("first listener's port still bound after failed New: %v", err)
	}
	_ = ln.Close()
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	cfg := mustCfg(t, doc("1s", tcpCfg("a", freeAddr(t), "")))
	cfg.Configurations[0].Access.Allow = []string{"not-a-cidr"}
	if _, err := New(Options{Config: cfg, Client: fake.NewSimpleClientset()}); err == nil {
		t.Fatal("New accepted an unparsable ACL")
	}
	if _, err := New(Options{Client: fake.NewSimpleClientset()}); err == nil {
		t.Fatal("New accepted a nil config")
	}
}

func TestShutdownOrder(t *testing.T) {
	port := echoBackend(t)
	var h *harness
	var addr string // set after Start; the hook only runs during Shutdown
	h = startRT(t, doc("2s", tcpCfg("web", anyAddr, "")), func(o *Options) {
		o.Sleep = func(ctx context.Context, d time.Duration) {
			h.log.add("sleep")
			// readiness has flipped but the listener must still accept.
			if c, err := net.DialTimeout("tcp", addr, time.Second); err != nil {
				h.log.add("listener-closed-during-delay")
			} else {
				_ = c.Close()
			}
		}
	}, tcpSvc("s", "web", port))
	addr = h.addr("web")
	_ = waitProxied(t, addr).Close()

	h.rt.Shutdown(context.Background())
	ev := h.log.snapshot()
	start := slices.Index(ev, "Ready(false)")
	if start < 0 {
		t.Fatalf("no Ready(false): %v", ev)
	}
	want := []string{"Ready(false)", "ready=false", "sleep", "readiness-delay", "discovery.stop", "drain", "pools.stop", "watchdog.stop"}
	if got := ev[start:]; !slices.Equal(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if ok, _ := h.rt.Ready(); ok {
		t.Fatal("ready after shutdown")
	}
	if refused(addr) == false {
		t.Fatal("listener still serving after shutdown")
	}
}

func TestReadyFalseUntilSynced(t *testing.T) {
	h := newRT(t, doc("1s", tcpCfg("web", anyAddr, "")), nil)
	release := block(t, h.cs, "services")

	done := make(chan error, 1)
	go func() { done <- h.rt.Start(context.Background()) }()

	time.Sleep(150 * time.Millisecond)
	if ok, why := h.rt.Ready(); ok || why == "" {
		t.Fatalf("Ready = %v, %q while discovery blocked", ok, why)
	}
	select {
	case err := <-done:
		t.Fatalf("Start returned early: %v", err)
	default:
	}

	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ok, why := h.rt.Ready(); !ok {
		t.Fatalf("not ready after sync: %s", why)
	}
	if h.log.index("Ready(true)") < 0 {
		t.Fatal("Recorder.Ready(true) never called")
	}
}

// TestServingButUnsyncedIsNotReady: Start gave up waiting (SyncTimeout,
// listeners serve), readiness stays false with its reason, connections get
// no_backend, and readiness flips once discovery syncs.
func TestServingButUnsyncedIsNotReady(t *testing.T) {
	h := newRT(t, doc("1s", tcpCfg("web", anyAddr, "")), func(o *Options) { o.SyncTimeout = 100 * time.Millisecond })
	release := block(t, h.cs, "services")

	if err := h.rt.Start(context.Background()); err != nil {
		t.Fatalf("Start after sync timeout = %v, want nil", err)
	}
	if ok, why := h.rt.Ready(); ok || why != "discovery not synced" {
		t.Fatalf("Ready = %v, %q, want false, \"discovery not synced\"", ok, why)
	}
	// Serving: the connection is accepted and then closed (no backend).
	if !refused(h.addr("web")) {
		t.Fatal("traffic proxied without endpoints")
	}
	c := dial(t, h.addr("web"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection stayed open with no backend")
	}
	eventually(t, "no_backend rejection", func() bool { return h.log.index("ConnRejected(web,no_backend)") >= 0 })

	release()
	eventually(t, "ready after late sync", func() bool { ok, _ := h.rt.Ready(); return ok })
}

func TestShutdownDuringStart(t *testing.T) {
	h := newRT(t, doc("1s", tcpCfg("web", anyAddr, "")), nil)
	release := block(t, h.cs, "services")
	started := make(chan error, 1)
	go func() { started <- h.rt.Start(context.Background()) }()
	time.Sleep(100 * time.Millisecond)

	down := make(chan struct{})
	go func() { h.rt.Shutdown(context.Background()); close(down) }()
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("Start succeeded after Shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
	release() // the fake list call ignores ctx; a real client aborts it
	select {
	case <-down:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not complete")
	}
	if _, err := h.rt.Reload(mustCfg(t, doc("1s", tcpCfg("web", anyAddr, "")))); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Reload after shutdown = %v", err)
	}
}

func TestDiscoveryEndpointsReachPool(t *testing.T) {
	port := echoBackend(t)
	h := startRT(t, doc("1s", tcpCfg("web", anyAddr, "")), nil)
	addr := h.addr("web")
	if !refused(addr) {
		t.Fatal("traffic proxied with no Service")
	}
	if _, err := h.cs.CoreV1().Services("default").Create(context.Background(), tcpSvc("s", "web", port), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitProxied(t, addr)

	// An unbound pool key and an unbound Service are ignored, not fatal.
	h.rt.poolsMu.RLock()
	n := len(h.rt.pools)
	h.rt.poolsMu.RUnlock()
	sink{h.rt}.SetEndpoints("nope", nil)
	if n != 1 {
		t.Fatalf("pools = %d", n)
	}
}

func TestUDPListenerServed(t *testing.T) {
	port := udpEchoBackend(t)
	h := startRT(t, doc("1s", udpCfg("dns", anyAddr)), nil, svc("s", "dns", port, corev1.ProtocolUDP, "dns"))
	addr := h.addr("dns")

	eventually(t, "udp echo", func() bool {
		c, err := net.Dial("udp", addr)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := c.Write([]byte("ping")); err != nil {
			return false
		}
		buf := make([]byte, 16)
		n, err := c.Read(buf)
		return err == nil && string(buf[:n]) == "ping"
	})
}

func TestShutdownForcesAfterTimeout(t *testing.T) {
	port := echoBackend(t)
	h := startRT(t, doc("200ms", tcpCfg("web", anyAddr, "")), nil, tcpSvc("s", "web", port))
	addr := h.addr("web")
	c := waitProxied(t, addr)

	begin := time.Now()
	sum := h.rt.Shutdown(context.Background())
	if sum.Forced != 1 {
		t.Fatalf("Forced = %d, want 1", sum.Forced)
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("forced connection still open")
	}
}

// TestShutdownCtxCancelForcesPromptly is the second-signal path: the drain
// timeout is long, the caller cancels, and both the live and the
// background-draining (replaced) listeners are force-closed at once.
func TestShutdownCtxCancelForcesPromptly(t *testing.T) {
	port := echoBackend(t)
	newAddr := freeAddr(t) // a reload that moves the listener needs a distinct address string
	h := startRT(t, doc("30s", tcpCfg("web", anyAddr, "")), nil, tcpSvc("s", "web", port))
	waitProxied(t, h.addr("web"))

	if _, err := h.rt.Reload(mustCfg(t, doc("30s", tcpCfg("web", newAddr, "")))); err != nil {
		t.Fatal(err)
	}
	waitProxied(t, newAddr)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	begin := time.Now()
	sum := h.rt.Shutdown(ctx)
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("shutdown took %v despite cancelled ctx", d)
	}
	if sum.Forced != 2 {
		t.Fatalf("Forced = %d, want 2 (live + background drain)", sum.Forced)
	}
}
