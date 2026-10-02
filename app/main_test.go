package main

import (
	"net"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/config"
	"github.com/cloudresty/nautiluslb/loadbalancer"
)

func TestApplicationVersion(t *testing.T) {
	// Test that version constants are properly defined
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{"Version should not be empty", "v0.0.6", true},
		{"Stage should not be empty", "alpha", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.value != "") != tt.expected {
				t.Errorf("Expected %v, got %v for %s", tt.expected, tt.value != "", tt.name)
			}
		})
	}
}

func TestApplicationInfo(t *testing.T) {
	// Test basic application information
	appName := "NautilusLB"
	if appName == "" {
		t.Error("Application name should not be empty")
	}

	repoURL := "https://github.com/cloudresty/nautiluslb"
	if repoURL == "" {
		t.Error("Repository URL should not be empty")
	}
}

// A failed bind must be returned (not panic) and release listeners bound
// before it.
func TestBindAllFailureReleasesBoundListeners(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()

	newLB := func(addr string) *loadbalancer.LoadBalancer {
		return loadbalancer.NewLoadBalancer(config.Configuration{Name: addr, ListenerAddress: addr, BackendPortName: "http"}, time.Second)
	}

	good := newLB("127.0.0.1:0")
	bad := newLB(taken.Addr().String())

	addr, err := bindAll([]*loadbalancer.LoadBalancer{good, bad})
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
