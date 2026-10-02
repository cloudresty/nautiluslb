package main

import (
	"net"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/tcpproxy"
)

// A failed bind must be returned (not panic) and release listeners bound
// before it.
func TestBindAllFailureReleasesBoundListeners(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()

	newLB := func(addr string) *tcpproxy.LoadBalancer {
		return tcpproxy.NewLoadBalancer(config.Configuration{Name: addr, ListenerAddress: addr, BackendPortName: "http"}, time.Second)
	}

	good := newLB("127.0.0.1:0")
	bad := newLB(taken.Addr().String())

	addr, err := bindAll([]*tcpproxy.LoadBalancer{good, bad})
	if err == nil || addr != taken.Addr().String() {
		t.Fatalf("bindAll = %q, %v; want failure on %s", addr, err, taken.Addr())
	}

	// The good listener was stopped: its port is closed again.
	if l := good.GetListener(); l != nil {
		if conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
			_ = conn.Close()
			t.Fatal("already-bound listener left open after failure")
		}
	}
}
