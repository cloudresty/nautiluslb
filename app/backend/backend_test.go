package backend

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	server := New(1, "192.168.1.1", 8080, "http")

	if server.ID != 1 || server.IP != "192.168.1.1" || server.Port != 8080 || server.PortName != "http" {
		t.Errorf("unexpected fields: %+v", server)
	}
	if server.Weight != 1 {
		t.Errorf("Weight = %d, want 1", server.Weight)
	}
	if !server.IsHealthy() {
		t.Error("a new backend should start healthy")
	}
	if server.ActiveConnections() != 0 {
		t.Errorf("ActiveConnections = %d, want 0", server.ActiveConnections())
	}
	if got := server.Address(); got != "192.168.1.1:8080" {
		t.Errorf("Address = %q", got)
	}
}

func TestAddressIPv6(t *testing.T) {
	if got := New(1, "fd00::1", 443, "https").Address(); got != "[fd00::1]:443" {
		t.Errorf("Address = %q", got)
	}
}

func TestSetHealthyReportsChange(t *testing.T) {
	server := New(1, "10.0.0.1", 80, "http")

	if server.SetHealthy(true) {
		t.Error("healthy -> healthy is not a change")
	}
	if !server.SetHealthy(false) {
		t.Error("healthy -> unhealthy is a change")
	}
	if server.healthStatus() != "unhealthy" {
		t.Errorf("status = %q", server.healthStatus())
	}
	if !server.SetHealthy(true) {
		t.Error("unhealthy -> healthy is a change")
	}
	if server.healthStatus() != "healthy" {
		t.Errorf("status = %q", server.healthStatus())
	}
}

func TestConnectionCounting(t *testing.T) {
	server := New(1, "10.0.0.1", 80, "http")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.Acquire()
			server.Release()
		}()
	}
	wg.Wait()

	if got := server.ActiveConnections(); got != 0 {
		t.Errorf("ActiveConnections = %d, want 0", got)
	}
}

func listen(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener, listener.Addr().(*net.TCPAddr).Port
}

func TestCheckOnceMarksUnhealthyAfterThreshold(t *testing.T) {
	listener, port := listen(t)
	_ = listener.Close() // nothing listens: every probe is refused

	server := New(1, "127.0.0.1", port, "http")
	ctx := context.Background()

	failures := 0
	for i := 1; i < unhealthyThreshold; i++ {
		failures = server.checkOnce(ctx, time.Second, failures)
		if !server.IsHealthy() {
			t.Fatalf("marked unhealthy after %d failures, threshold is %d", i, unhealthyThreshold)
		}
	}

	failures = server.checkOnce(ctx, time.Second, failures)
	if server.IsHealthy() {
		t.Fatalf("still healthy after %d failures", failures)
	}
}

func TestCheckOnceRecovers(t *testing.T) {
	listener, port := listen(t)
	defer func() { _ = listener.Close() }()

	server := New(1, "127.0.0.1", port, "http")
	server.SetHealthy(false)

	if failures := server.checkOnce(context.Background(), time.Second, 5); failures != 0 {
		t.Errorf("failures after success = %d, want 0", failures)
	}
	if !server.IsHealthy() {
		t.Error("a successful probe should restore health")
	}
}

func TestCheckOnceIgnoresShutdown(t *testing.T) {
	server := New(1, "192.0.2.1", 80, "http")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for range unhealthyThreshold + 1 {
		if failures := server.checkOnce(ctx, time.Second, 0); failures != 0 {
			t.Fatalf("a probe cancelled by shutdown counted as a failure")
		}
	}
	if !server.IsHealthy() {
		t.Error("shutdown must not mark a backend unhealthy")
	}
}

func TestHealthCheckStopsOnCancel(t *testing.T) {
	listener, port := listen(t)
	defer func() { _ = listener.Close() }()

	server := New(1, "127.0.0.1", port, "http")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.HealthCheck(ctx, 10*time.Millisecond, time.Second)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HealthCheck did not return after cancel")
	}
}

func TestHealthCheckProbeIsBounded(t *testing.T) {
	// A probe to a blackholed address must give up after the timeout.
	server := New(1, "192.0.2.1", 9, "http")

	start := time.Now()
	server.checkOnce(context.Background(), 100*time.Millisecond, 0)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probe took %v with a 100ms timeout", elapsed)
	}
}

func TestPortFormatting(t *testing.T) {
	server := New(1, "10.0.0.1", 30554, "https")
	if server.Address() != "10.0.0.1:"+strconv.Itoa(30554) {
		t.Errorf("Address = %q", server.Address())
	}
}
