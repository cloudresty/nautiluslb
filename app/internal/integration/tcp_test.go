//go:build integration

package integration

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/proxyproto"
)

func TestTCPRoundRobin(t *testing.T) {
	addr := anyAddr
	a, b, c := newBackend(t, "A"), newBackend(t, "B"), newBackend(t, "C")
	e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone)), nil,
		tcpSvc(a, "web"), tcpSvc(b, "web"), tcpSvc(c, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 3)

	got := map[string]int{}
	for range 30 {
		cl, id := connectID(t, addr)
		if !cl.echo("ping") {
			t.Fatalf("no echo through backend %s", id)
		}
		got[id]++
		_ = cl.Close()
	}
	for _, id := range []string{"A", "B", "C"} {
		if got[id] != 10 {
			t.Fatalf("distribution = %v, want exactly 10 each", got)
		}
	}
	e.waitMetric("30 accepted connections", 30, mp+"connections_accepted_total", "listener", "web")
	if n := e.metric(mp+"connections_accepted_total", "listener", "web"); n != 30 {
		t.Fatalf("accepted = %v, want 30", n)
	}
}

func TestLeastConn(t *testing.T) {
	addr := anyAddr
	a, b, c := newBackend(t, "A"), newBackend(t, "B"), newBackend(t, "C")
	e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone+"    balancer: {algorithm: least_conn}\n")), nil,
		tcpSvc(a, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 1)

	// Two long connections on A (the only backend so far).
	for range 2 {
		if _, id := connectID(t, addr); id != "A" {
			t.Fatalf("held connection served by %s, want A", id)
		}
	}

	e.setService(tcpSvc(b, "web"))
	e.setService(tcpSvc(c, "web"))
	e.waitPool("web", "web", 3)

	// A has 2 active; B and C have 0. Four held connections must split 2/2
	// between B and C and never touch A.
	got := map[string]int{}
	for range 4 {
		_, id := connectID(t, addr)
		got[id]++
	}
	if got["A"] != 0 || got["B"] != 2 || got["C"] != 2 {
		t.Fatalf("least_conn distribution of new connections = %v, want B:2 C:2 A:0", got)
	}
}

// sendProxyV2 sends a PROXY v2 header claiming src to the balancer.
func sendProxyV2(t *testing.T, c net.Conn, src string, dst net.Addr) {
	t.Helper()
	sa, err := net.ResolveTCPAddr("tcp", src)
	if err != nil {
		t.Fatal(err)
	}
	if err := proxyproto.WriteHeader(c, proxyproto.V2, sa, dst); err != nil {
		t.Fatalf("writing PROXY header: %v", err)
	}
}

func TestSourceIPHashAffinity(t *testing.T) {
	addr := anyAddr
	a, b, c := newBackend(t, "A"), newBackend(t, "B"), newBackend(t, "C")
	cfg := tcpConf("web", addr, healthNone+
		"    balancer: {algorithm: source_ip_hash}\n"+
		"    proxyProtocol: {in: {trustedCIDRs: [\"127.0.0.0/8\"], required: true}}\n")
	e := startEnv(t, doc("2s", cfg), nil, tcpSvc(a, "web"), tcpSvc(b, "web"), tcpSvc(c, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 3)
	dst, _ := net.ResolveTCPAddr("tcp", addr)

	via := func(ip string, port int) string {
		t.Helper()
		cl := dialLB(t, addr)
		sendProxyV2(t, cl, fmt.Sprintf("%s:%d", ip, port), dst)
		id, err := cl.id()
		if err != nil {
			t.Fatalf("client %s: %v", ip, err)
		}
		_ = cl.Close()
		return id
	}

	// Same effective client IP (different source ports): one backend.
	first := via("198.51.100.7", 1000)
	for i := 1; i < 20; i++ {
		if id := via("198.51.100.7", 1000+i); id != first {
			t.Fatalf("connection %d of 198.51.100.7 went to %s, first went to %s", i, id, first)
		}
	}

	// Many distinct IPs spread over more than one backend, each one sticky.
	spread := map[string]int{}
	for i := 1; i <= 48; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i)
		id := via(ip, 5000)
		if again := via(ip, 5001); again != id {
			t.Fatalf("%s moved from %s to %s", ip, id, again)
		}
		spread[id]++
	}
	if len(spread) < 2 {
		t.Fatalf("48 distinct client IPs all hashed to one backend: %v", spread)
	}
}

func TestProxyProtocolInOut(t *testing.T) {
	inout, plain := anyAddr, anyAddr
	be := startBackend(t, "P", modeProxy, nil)
	yaml := doc("2s",
		tcpConf("inout", inout, healthNone+
			"    proxyProtocol: {in: {trustedCIDRs: [\"127.0.0.0/8\"]}, out: v1}\n"),
		tcpConf("plain", plain, healthNone+
			"    proxyProtocol: {in: {trustedCIDRs: [\"10.0.0.0/8\"]}, out: v1}\n"))
	e := startEnv(t, yaml, nil, tcpSvc(be, "inout,plain"))
	inout, plain = e.addr("inout"), e.addr("plain")
	e.waitPool("inout", "inout", 1)
	e.waitPool("plain", "plain", 1)

	t.Run("trusted inbound v2 to outbound v1", func(t *testing.T) {
		dst, _ := net.ResolveTCPAddr("tcp", inout)
		cl := dialLB(t, inout)
		sendProxyV2(t, cl, "203.0.113.9:4242", dst)
		got, err := cl.line()
		if err != nil {
			t.Fatal(err)
		}
		if want := "v1 src=203.0.113.9:4242"; got != want {
			t.Fatalf("backend saw %q, want %q (the original client from the inbound header)", got, want)
		}
		if !cl.echo("payload\n") {
			t.Fatal("payload after the headers was not echoed")
		}
		_ = cl.Close() // the access log record is written when the connection ends
		eventually(t, "access log record with proxySrc", func() bool {
			r, ok := e.log.find(func(r accesslogRecord) bool { return r.Listener == "inout" && r.ProxySrc == "203.0.113.9:4242" })
			return ok && r.Client != ""
		})
	})

	t.Run("untrusted peer cannot spoof its address", func(t *testing.T) {
		cl := dialLB(t, plain)
		// 127.0.0.1 is not trusted here: the PROXY line is just payload.
		if _, err := cl.Write([]byte("PROXY TCP4 203.0.113.9 127.0.0.1 1 2\r\n")); err != nil {
			t.Fatal(err)
		}
		got, err := cl.line()
		if err != nil {
			t.Fatal(err)
		}
		if want := "v1 src=" + cl.LocalAddr().String(); got != want {
			t.Fatalf("backend saw %q, want the real peer %q", got, want)
		}
	})
}

func TestACLDeny(t *testing.T) {
	addr := anyAddr
	be := newBackend(t, "A")
	e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone+"    access: {deny: [\"127.0.0.1/32\"]}\n")), nil, tcpSvc(be, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 1)

	cl := dialLB(t, addr)
	start := time.Now()
	if id, err := cl.id(); err == nil {
		t.Fatalf("denied client got a backend greeting %q", id)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("denied connection was closed after %v, want immediately", d)
	}
	e.waitMetric("rejected{acl}", 1, mp+"connections_rejected_total", "listener", "web", "reason", "acl")
	if n := e.metric(mp + "connections_accepted_total"); n != 0 {
		t.Fatalf("accepted = %v, want 0 for an ACL-denied connection", n)
	}
	if be.accepted.Load() != 0 {
		t.Fatal("backend was dialed for a denied client")
	}
	eventually(t, "acl access log record", func() bool {
		_, ok := e.log.find(func(r accesslogRecord) bool { return r.Listener == "web" && r.Result == "acl" })
		return ok
	})
}
