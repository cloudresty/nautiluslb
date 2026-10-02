//go:build integration

package integration

import (
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestUDPEcho(t *testing.T) {
	addr := anyAddr
	be := newUDPBackend(t)
	e := startEnv(t, doc("2s", udpConf("dns", addr)), nil,
		svc("svc-dns", "dns", be.port, corev1.ProtocolUDP, "dns"))
	addr = e.addr("dns")
	e.waitPool("dns", "dns", 1)

	laddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip := func(c *net.UDPConn, msg string) {
		t.Helper()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil || string(buf[:n]) != msg {
			t.Fatalf("echo of %q = %q, %v", msg, buf[:n], err)
		}
	}

	c1, err := net.DialUDP("udp", nil, laddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c1.Close() }()
	for _, m := range []string{"one", "two", "three", "four", "five"} {
		roundTrip(c1, m)
	}
	// One client socket: one session, so the backend saw one source throughout.
	if n := e.metric(mp+"udp_sessions_total", "listener", "dns", "event", "open"); n != 1 {
		t.Fatalf("sessions opened after 5 datagrams from one socket = %v, want 1 (reuse)", n)
	}
	if n := be.distinctSources(); n != 1 {
		t.Fatalf("backend saw %d distinct sources, want 1", n)
	}

	// A second client socket is a second session.
	c2, err := net.DialUDP("udp", nil, laddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	roundTrip(c2, "other")
	e.waitMetric("2 sessions", 2, mp+"udp_sessions_total", "listener", "dns", "event", "open")
	if n := e.metric(mp+"udp_sessions_total", "listener", "dns", "event", "open"); n != 2 {
		t.Fatalf("sessions opened = %v, want 2", n)
	}
	if n := be.distinctSources(); n != 2 {
		t.Fatalf("backend saw %d distinct sources, want 2", n)
	}
	if n := e.metric(mp+"udp_datagrams_total", "listener", "dns"); n < 12 {
		t.Fatalf("udp_datagrams_total = %v, want >= 12 (6 each way)", n)
	}
}
