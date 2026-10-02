//go:build integration

package integration

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	nlb "github.com/cloudresty/nautiluslb/internal/runtime"
)

func TestHotReload(t *testing.T) {
	be := newBackend(t, "A")
	base := func(webExtra string) string { return tcpConf("web", anyAddr, healthNone+webExtra) }
	e := startEnv(t, doc("2s", base("")), nil, tcpSvc(be, "web,extra"))
	web := e.addr("web")
	var extra string // bound address of the listener added by a reload
	e.waitPool("web", "web", 1)

	// Existing connection used across the whole test.
	long, id := connectID(t, web)
	if id != "A" || !long.echo("hi") {
		t.Fatal("initial connection does not work")
	}

	t.Run("acl change applies in place", func(t *testing.T) {
		sum, err := e.reload(doc("2s", base("    access: {deny: [\"127.0.0.1/32\"]}\n")))
		if err != nil || !slices.Equal(sum.Updated, []string{"web"}) {
			t.Fatalf("Reload = %+v, %v; want Updated=[web]", sum, err)
		}
		if !long.echo("still") {
			t.Fatal("connection accepted before the reload did not survive the in-place update")
		}
		if !rejected(web) {
			t.Fatal("new connection was served although the new ACL denies it")
		}
		e.waitMetric("rejected{acl}", 1, mp+"connections_rejected_total", "listener", "web", "reason", "acl")

		if _, err := e.reload(doc("2s", base(""))); err != nil {
			t.Fatal(err)
		}
		if _, id := connectID(t, web); id != "A" {
			t.Fatalf("after re-allowing, served by %q", id)
		}
	})

	t.Run("added listener serves", func(t *testing.T) {
		sum, err := e.reload(doc("2s", base(""), tcpConf("extra", anyAddr, healthNone)))
		if err != nil || !slices.Equal(sum.Added, []string{"extra"}) {
			t.Fatalf("Reload = %+v, %v; want Added=[extra]", sum, err)
		}
		extra = e.addr("extra")
		e.waitPool("extra", "extra", 1)
		if _, id := connectID(t, extra); id != "A" {
			t.Fatalf("new listener served by %q", id)
		}
	})

	t.Run("removed listener drains", func(t *testing.T) {
		held, _ := connectID(t, extra)
		sum, err := e.reload(doc("2s", base("")))
		if err != nil || !slices.Equal(sum.Removed, []string{"extra"}) {
			t.Fatalf("Reload = %+v, %v; want Removed=[extra]", sum, err)
		}
		eventually(t, "new connections to the removed listener to be refused", func() bool {
			c, err := net.DialTimeout("tcp", extra, time.Second)
			if err != nil {
				return true
			}
			_ = c.Close()
			return false
		})
		if !held.echo("draining") {
			t.Fatal("open connection of the removed listener was cut instead of drained")
		}
		if e.rt.WaitDrained(100 * time.Millisecond) {
			t.Fatal("drain finished while a connection was still open")
		}
		_ = held.Close()
		if !e.rt.WaitDrained(3 * time.Second) {
			t.Fatal("drain did not finish after the last connection closed")
		}
		if !long.echo("web survived") {
			t.Fatal("unrelated listener affected by removing another")
		}
	})

	t.Run("invalid reload is rejected atomically", func(t *testing.T) {
		busy, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = busy.Close() }()
		// Valid on its own, but binding "bad" fails: the ACL change on web
		// in the same document must not be applied either.
		bad := doc("2s",
			base("    access: {deny: [\"127.0.0.1/32\"]}\n"),
			tcpConf("bad", busy.Addr().String(), healthNone))
		if _, err := e.reload(bad); err == nil {
			t.Fatal("Reload succeeded although a listener address is already in use")
		}
		e.waitMetric("config_reload_total{rejected}", 1, mp+"config_reload_total", "result", "rejected")
		if _, id := connectID(t, web); id != "A" {
			t.Fatalf("after rejected reload, new connection served by %q", id)
		}
		if !long.echo("old config") {
			t.Fatal("existing connection broken by a rejected reload")
		}
		if n := e.metric(mp+"config_reload_total", "result", "applied"); n < 4 {
			t.Fatalf("applied reloads = %v, want >= 4", n)
		}
	})
}

// gatedSleep replaces Shutdown's readiness-delay wait: it signals that the
// delay started and blocks until released (or ctx ends).
type gatedSleep struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func newGatedSleep() *gatedSleep {
	return &gatedSleep{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedSleep) sleep(ctx context.Context, _ time.Duration) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
}

// open releases the gate; safe to call any number of times.
func (g *gatedSleep) open() { g.relOnce.Do(func() { close(g.release) }) }

// waitStarted fails the test if Shutdown does not reach the readiness delay.
func (g *gatedSleep) waitStarted(t testing.TB) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not reach the readiness delay")
	}
}

func (g *gatedSleep) opt(o *nlb.Options) { o.Sleep = g.sleep }

func TestDrainWithOpenConnection(t *testing.T) {
	t.Run("connection that finishes in time", func(t *testing.T) {
		addr := anyAddr
		be := newBackend(t, "A")
		g := newGatedSleep()
		e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone)), g.opt, tcpSvc(be, "web"))
		// Registered after startEnv, so it runs before the forced Shutdown of
		// its cleanup: a failed assertion must not leave Shutdown blocked on
		// the gate while holding the runtime mutex.
		t.Cleanup(g.open)
		addr = e.addr("web")
		e.waitPool("web", "web", 1)

		cl, _ := connectID(t, addr)
		type result struct {
			sum nlb.Summary
			dur time.Duration
		}
		done := make(chan result, 1)
		t0 := time.Now()
		go func() {
			s := e.rt.Shutdown(context.Background())
			done <- result{s, time.Since(t0)}
		}()

		g.waitStarted(t)
		if ok, why := e.rt.Ready(); ok || why == "" {
			t.Fatalf("Ready() = %v, %q during the readiness delay, want false with a reason", ok, why)
		}
		// Readiness flipped before any listener closed: the listener still accepts.
		probe, id := connectID(t, addr)
		if id != "A" {
			t.Fatalf("listener closed before the readiness delay ended (id %q)", id)
		}
		_ = probe.Close() // else it would be an open connection that the drain has to force

		// The open connection keeps sending slowly across the drain.
		finished := make(chan bool, 1)
		go func() {
			ok := true
			for range 6 {
				ok = ok && cl.echo("x")
				time.Sleep(100 * time.Millisecond)
			}
			finished <- ok
			_ = cl.Close()
		}()
		time.Sleep(150 * time.Millisecond)
		g.open()

		eventually(t, "listener to stop accepting during the drain", func() bool {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				return true
			}
			_ = c.Close()
			return false
		})
		select {
		case ok := <-finished:
			if !ok {
				t.Fatal("slow connection was cut during the drain")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("slow client did not finish")
		}
		var r result
		select {
		case r = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Shutdown did not return")
		}
		if r.sum.Forced != 0 {
			t.Fatalf("Forced = %d, want 0 (the connection completed within the drain timeout)", r.sum.Forced)
		}
		if r.dur >= 2*time.Second {
			t.Fatalf("Shutdown took %v, want less than the 2s drain timeout", r.dur)
		}
	})

	t.Run("connection that outlives the drain timeout is forced", func(t *testing.T) {
		addr := anyAddr
		be := newBackend(t, "A")
		e := startEnv(t, doc("1s", tcpConf("web", addr, healthNone)), nil, tcpSvc(be, "web"))
		addr = e.addr("web")
		e.waitPool("web", "web", 1)

		cl, _ := connectID(t, addr)
		t0 := time.Now()
		sum := e.rt.Shutdown(context.Background())
		d := time.Since(t0)
		if sum.Forced != 1 {
			t.Fatalf("Forced = %d, want 1", sum.Forced)
		}
		if d < 900*time.Millisecond || d > 5*time.Second {
			t.Fatalf("Shutdown took %v, want about the 1s drain timeout", d)
		}
		if cl.echo("late") {
			t.Fatal("forced connection still works")
		}
		if ok, _ := e.rt.Ready(); ok {
			t.Fatal("ready after shutdown")
		}
	})
}
