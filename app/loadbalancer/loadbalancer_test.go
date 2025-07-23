package loadbalancer

import (
	"sync"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/backend"
	"github.com/cloudresty/nautiluslb/config"
)

func TestNewLoadBalancer(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
		Namespace:       "default",
	}

	timeout := 30 * time.Second
	lb := NewLoadBalancer(cfg, timeout)

	if lb == nil {
		t.Fatal("NewLoadBalancer should not return nil")
	}

	if lb.config.Name != "test-lb" {
		t.Errorf("Expected config name 'test-lb', got '%s'", lb.config.Name)
	}

	if lb.requestTimeout != timeout {
		t.Errorf("Expected request timeout %v, got %v", timeout, lb.requestTimeout)
	}

	if lb.stopChan == nil {
		t.Error("stopChan should be initialized")
	}

	if lb.stopHealthChecks == nil {
		t.Error("stopHealthChecks should be initialized")
	}

	if lb.healthCheckMap == nil {
		t.Error("healthCheckMap should be initialized")
	}

	if lb.healthCheckCache == nil {
		t.Error("healthCheckCache should be initialized")
	}

	// Note: portBackendMap is not initialized in NewLoadBalancer - this is expected behavior
}

func TestLoadBalancerGetters(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	// Test GetMu
	mu := lb.GetMu()
	if mu == nil {
		t.Error("GetMu should not return nil")
	}

	// Test GetBackendServers
	servers := lb.GetBackendServers()
	if servers == nil {
		t.Error("GetBackendServers should not return nil")
	}

	if len(servers) != 0 {
		t.Errorf("Expected 0 backend servers initially, got %d", len(servers))
	}
}

func TestSetBackendServers(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	servers := []*backend.BackendServer{
		{
			ID:       1,
			IP:       "192.168.1.1",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
		{
			ID:       2,
			IP:       "192.168.1.2",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
	}

	lb.SetBackendServers(servers)

	retrievedServers := lb.GetBackendServers()
	if len(retrievedServers) != 2 {
		t.Errorf("Expected 2 backend servers, got %d", len(retrievedServers))
	}

	if retrievedServers[0].ID != 1 {
		t.Errorf("Expected first server ID 1, got %d", retrievedServers[0].ID)
	}

	if retrievedServers[1].IP != "192.168.1.2" {
		t.Errorf("Expected second server IP '192.168.1.2', got '%s'", retrievedServers[1].IP)
	}
}

func TestGetNextBackend(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	servers := []*backend.BackendServer{
		{
			ID:       1,
			IP:       "192.168.1.1",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
		{
			ID:       2,
			IP:       "192.168.1.2",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
	}

	lb.SetBackendServers(servers)

	// Test round-robin selection with healthy servers
	backend1 := lb.getNextBackend()
	if backend1 == nil {
		t.Fatal("getNextBackend should not return nil when healthy servers exist")
	}

	backend2 := lb.getNextBackend()
	if backend2 == nil {
		t.Fatal("getNextBackend should not return nil when healthy servers exist")
	}

	backend3 := lb.getNextBackend()
	if backend3 == nil {
		t.Fatal("getNextBackend should not return nil when healthy servers exist")
	}

	// Verify round-robin behavior (should cycle through healthy servers)
	if backend1.ID == backend2.ID {
		t.Error("Round-robin should select different servers")
	}

	// Third call should return back to the first server (round-robin)
	if backend1.ID != backend3.ID {
		t.Error("Round-robin should cycle back to first server on third call")
	}
}

func TestGetNextBackendNoHealthyServers(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	servers := []*backend.BackendServer{
		{
			ID:       1,
			IP:       "192.168.1.1",
			Port:     8080,
			PortName: "http",
			Healthy:  false,
		},
		{
			ID:       2,
			IP:       "192.168.1.2",
			Port:     8080,
			PortName: "http",
			Healthy:  false,
		},
	}

	lb.SetBackendServers(servers)

	// Use a timeout to prevent hanging
	done := make(chan *backend.BackendServer, 1)
	go func() {
		backend := lb.getNextBackend()
		done <- backend
	}()

	select {
	case backend := <-done:
		if backend != nil {
			t.Error("getNextBackend should return nil when no healthy servers exist")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("getNextBackend timed out - it should return quickly when no healthy servers exist")
	}
}

func TestGetNextBackendWithUnhealthyServers(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	servers := []*backend.BackendServer{
		{
			ID:       1,
			IP:       "192.168.1.1",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
		{
			ID:       2,
			IP:       "192.168.1.2",
			Port:     8080,
			PortName: "http",
			Healthy:  false, // Unhealthy server
		},
		{
			ID:       3,
			IP:       "192.168.1.3",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
	}

	lb.SetBackendServers(servers)

	// Use timeout to prevent hanging
	done := make(chan *backend.BackendServer, 1)
	go func() {
		backend := lb.getNextBackend()
		done <- backend
	}()

	select {
	case backend := <-done:
		if backend == nil {
			t.Fatal("getNextBackend should return a healthy server when healthy servers exist")
		}
		if !backend.Healthy {
			t.Error("getNextBackend should only return healthy servers")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("getNextBackend timed out - it should return quickly when healthy servers exist")
	}
}

func TestGetNextBackendEmptyServers(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	backend := lb.getNextBackend()
	if backend != nil {
		t.Error("getNextBackend should return nil when no servers exist")
	}
}

func TestStopHealthChecks(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	// Test stopping health checks
	lb.StopHealthChecks()

	if !lb.areHealthChecksStopped() {
		t.Error("Health checks should be stopped after StopHealthChecks()")
	}
}

func TestLoadBalancerStop(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	// Test stopping the load balancer
	lb.Stop()

	// Verify stop channel is closed by checking if we can receive from it
	select {
	case <-lb.stopChan:
		// Expected - channel should be closed
	default:
		t.Error("stopChan should be closed after Stop()")
	}
}

func TestLoadBalancerConcurrency(t *testing.T) {
	cfg := config.Configuration{
		Name:            "test-lb",
		ListenerAddress: ":8080",
		RequestTimeout:  30,
		BackendPortName: "http",
	}

	lb := NewLoadBalancer(cfg, 30*time.Second)

	servers := []*backend.BackendServer{
		{
			ID:       1,
			IP:       "192.168.1.1",
			Port:     8080,
			PortName: "http",
			Healthy:  true,
		},
	}

	lb.SetBackendServers(servers)

	// Test concurrent access to getNextBackend
	var wg sync.WaitGroup
	results := make(chan *backend.BackendServer, 10)

	// Use a timeout to prevent hanging
	done := make(chan bool, 1)

	go func() {
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				backend := lb.getNextBackend()
				results <- backend
			}()
		}

		wg.Wait()
		close(results)
		done <- true
	}()

	select {
	case <-done:
		// Verify all goroutines got valid results
		count := 0
		for backend := range results {
			if backend == nil {
				t.Error("Concurrent getNextBackend should not return nil")
			}
			count++
		}

		if count != 10 {
			t.Errorf("Expected 10 results, got %d", count)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Concurrent test timed out")
	}
}
