package runtime

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"
)

func TestReloadUnchangedUntouched(t *testing.T) {
	port := echoBackend(t)
	yaml := doc("1s", tcpCfg("web", anyAddr, ""))
	h := startRT(t, yaml, nil, tcpSvc("s", "web", port))
	conn := waitProxied(t, h.addr("web"))

	lis, p := h.rt.entries["web"].tcp, h.rt.pools["web"]
	sum, err := h.rt.Reload(mustCfg(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Unchanged, []string{"web"}) || len(sum.Added)+len(sum.Updated)+len(sum.Removed)+len(sum.Replaced) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if h.rt.entries["web"].tcp != lis || h.rt.pools["web"] != p {
		t.Fatal("unchanged listener or pool was replaced")
	}
	if !echo(conn, "still") {
		t.Fatal("open connection did not survive the reload")
	}
	if h.log.index("ConfigReload(unchanged)") < 0 {
		t.Fatalf("log = %v", h.log.snapshot())
	}

	// A settings-only change is reported, not applied.
	sum, err = h.rt.Reload(mustCfg(t, docWith("1s", "  logLevel: debug\n", tcpCfg("web", anyAddr, ""))))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sum.Ignored, "settings.logLevel") {
		t.Fatalf("Ignored = %v", sum.Ignored)
	}
	if h.rt.entries["web"].tcp != lis {
		t.Fatal("settings change touched the listener")
	}
}

func TestReloadUpdatedInPlace(t *testing.T) {
	port := echoBackend(t)
	h := startRT(t, doc("1s", tcpCfg("web", anyAddr, "")), nil, tcpSvc("s", "web", port))
	addr := h.addr("web")
	old := waitProxied(t, addr)
	lis, p := h.rt.entries["web"].tcp, h.rt.pools["web"]

	deny := "    access: {deny: [127.0.0.1/32]}\n"
	sum, err := h.rt.Reload(mustCfg(t, doc("1s", tcpCfg("web", anyAddr, deny))))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Updated, []string{"web"}) {
		t.Fatalf("summary = %+v", sum)
	}
	if h.rt.entries["web"].tcp != lis || h.rt.pools["web"] != p {
		t.Fatal("in-place update replaced the listener or pool")
	}
	if !echo(old, "kept") {
		t.Fatal("existing connection broken by ACL update")
	}
	if !refused(addr) {
		t.Fatal("new connection allowed after deny ACL")
	}

	if _, err := h.rt.Reload(mustCfg(t, doc("1s", tcpCfg("web", anyAddr, "")))); err != nil {
		t.Fatal(err)
	}
	waitProxied(t, addr)
}

func TestReloadReplacedBindsFirst(t *testing.T) {
	port := echoBackend(t)
	// The replacement needs a configured address string different from the
	// running one, so it takes a pre-reserved port.
	newAddr := freeAddr(t)
	h := startRT(t, doc("5s", tcpCfg("web", anyAddr, "")), nil, tcpSvc("s", "web", port))
	oldAddr := h.addr("web")
	old := waitProxied(t, oldAddr)
	p := h.rt.pools["web"]

	sum, err := h.rt.Reload(mustCfg(t, doc("5s", tcpCfg("web", newAddr, ""))))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Replaced, []string{"web"}) {
		t.Fatalf("summary = %+v", sum)
	}
	if bound, drain := h.log.index("reload.bound"), h.log.index("reload.drain.begin"); bound < 0 || (drain >= 0 && drain < bound) {
		t.Fatalf("new address must be bound before the old drains: %v", h.log.snapshot())
	}
	waitProxied(t, newAddr) // endpoints survived the move: the pool is reused
	if h.rt.pools["web"] != p {
		t.Fatal("pool of a replaced configuration was rebuilt")
	}
	if !echo(old, "finishing") {
		t.Fatal("old connection did not finish on the old listener")
	}
	eventually(t, "old address closed", func() bool {
		c, err := net.DialTimeout("tcp", oldAddr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	})
	_ = old.Close()
	if !h.rt.WaitDrained(5 * time.Second) {
		t.Fatal("background drain did not finish")
	}
}

func TestReloadRejectedKeepsOld(t *testing.T) {
	port := echoBackend(t)
	h := startRT(t, doc("1s", tcpCfg("web", anyAddr, ""), tcpCfg("api", anyAddr, "")), nil, tcpSvc("s", "web,api", port))
	addr := h.addr("web")
	waitProxied(t, addr)
	lis := h.rt.entries["web"].tcp

	// Bind conflict: "fresh" binds fine, "clash" does not; nothing is applied
	// and "fresh"'s socket is released again.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	fresh := freeAddr(t) // the released port is checked afterwards
	deny := "    access: {deny: [127.0.0.1/32]}\n"
	_, err = h.rt.Reload(mustCfg(t, doc("1s",
		tcpCfg("web", anyAddr, deny), tcpCfg("api", anyAddr, ""),
		tcpCfg("fresh", fresh, ""), tcpCfg("clash", taken.Addr().String(), ""))))
	if err == nil {
		t.Fatal("reload with a bind conflict succeeded")
	}
	if h.log.index("ConfigReload(rejected)") < 0 {
		t.Fatalf("log = %v", h.log.snapshot())
	}
	if ln, err := net.Listen("tcp", fresh); err != nil {
		t.Fatalf("bound-then-rejected listener leaked: %v", err)
	} else {
		_ = ln.Close()
	}
	if len(h.rt.entries) != 2 || h.rt.entries["web"].tcp != lis {
		t.Fatal("running state changed by a rejected reload")
	}
	waitProxied(t, addr) // the deny ACL of the rejected reload was not applied

	// Invalid in-place update (bad ACL) after a valid one: all or nothing.
	bad := mustCfg(t, doc("1s", tcpCfg("web", anyAddr, deny), tcpCfg("api", anyAddr, "")))
	bad.Configurations[1].Access.Allow = []string{"not-a-cidr"}
	if _, err := h.rt.Reload(bad); err == nil {
		t.Fatal("reload with an invalid ACL succeeded")
	}
	waitProxied(t, addr)
	if _, err := h.rt.Reload(nil); err == nil {
		t.Fatal("nil reload succeeded")
	}
}

func TestReloadRemovedDrains(t *testing.T) {
	port := echoBackend(t)
	h := startRT(t, doc("5s", tcpCfg("a", anyAddr, ""), tcpCfg("b", anyAddr, "")), nil, tcpSvc("s", "a,b", port))
	a, b := h.addr("a"), h.addr("b")
	waitProxied(t, a)
	conn := waitProxied(t, b)

	sum, err := h.rt.Reload(mustCfg(t, doc("5s", tcpCfg("a", anyAddr, ""))))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Removed, []string{"b"}) || !slices.Equal(sum.Unchanged, []string{"a"}) {
		t.Fatalf("summary = %+v", sum)
	}
	eventually(t, "b stops accepting", func() bool { return refused(b) })
	if !echo(conn, "inflight") {
		t.Fatal("in-flight connection of a removed listener was cut")
	}
	if _, ok := h.rt.pools["b"]; ok {
		t.Fatal("pool of the removed configuration still registered")
	}
	_ = conn.Close()
	if !h.rt.WaitDrained(5 * time.Second) {
		t.Fatal("background drain did not finish")
	}
	if h.log.index("reload.drain.end") < 0 {
		t.Fatalf("log = %v", h.log.snapshot())
	}
	waitProxied(t, a)
}

func TestReloadAddedServes(t *testing.T) {
	port := echoBackend(t)
	h := startRT(t, doc("1s", tcpCfg("a", anyAddr, "")), nil, tcpSvc("s", "a,b", port))
	waitProxied(t, h.addr("a"))
	sum, err := h.rt.Reload(mustCfg(t, doc("1s", tcpCfg("a", anyAddr, ""), tcpCfg("b", anyAddr, ""))))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Added, []string{"b"}) {
		t.Fatalf("summary = %+v", sum)
	}
	waitProxied(t, h.addr("b")) // a brand-new pool gets its endpoints through Rebind
}

func TestReloadSerialisedWithShutdown(t *testing.T) {
	port := echoBackend(t)
	b := freeAddr(t) // checked for release after Shutdown, so it must be known up front
	entered, release := make(chan struct{}), make(chan struct{})
	h := startRT(t, doc("2s", tcpCfg("a", anyAddr, "")), func(o *Options) {
		base := o.trace
		o.trace = func(step string) {
			base(step)
			if step == "reload.bound" {
				close(entered)
				<-release
			}
		}
	}, tcpSvc("s", "a,b", port))
	waitProxied(t, h.addr("a"))

	reloaded := make(chan error, 1)
	go func() {
		_, err := h.rt.Reload(mustCfg(t, doc("2s", tcpCfg("a", anyAddr, ""), tcpCfg("b", b, ""))))
		reloaded <- err
	}()
	<-entered

	down := make(chan Summary, 1)
	go func() { down <- h.rt.Shutdown(context.Background()) }()
	select {
	case <-down:
		t.Fatal("Shutdown ran while a Reload held the runtime")
	case <-time.After(150 * time.Millisecond):
	}
	if ok, _ := h.rt.Ready(); !ok {
		t.Fatal("readiness flipped before Shutdown got the mutex")
	}

	close(release)
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	select {
	case <-down:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never completed")
	}
	// The listener the reload added was drained too, not leaked.
	if ln, err := net.Listen("tcp", b); err != nil {
		t.Fatalf("listener added by the reload survived Shutdown: %v", err)
	} else {
		_ = ln.Close()
	}
	if _, err := h.rt.Reload(mustCfg(t, doc("2s", tcpCfg("a", anyAddr, "")))); err == nil {
		t.Fatal("Reload after Shutdown succeeded")
	}
}
