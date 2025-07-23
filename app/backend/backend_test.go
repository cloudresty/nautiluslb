package backend

import (
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBackendServerCreation(t *testing.T) {
	server := &BackendServer{
		ID:       1,
		IP:       "192.168.1.1",
		Port:     8080,
		PortName: "http",
		Weight:   100,
		Healthy:  true,
	}

	if server.ID != 1 {
		t.Errorf("Expected ID 1, got %d", server.ID)
	}

	if server.IP != "192.168.1.1" {
		t.Errorf("Expected IP '192.168.1.1', got '%s'", server.IP)
	}

	if server.Port != 8080 {
		t.Errorf("Expected Port 8080, got %d", server.Port)
	}

	if server.PortName != "http" {
		t.Errorf("Expected PortName 'http', got '%s'", server.PortName)
	}

	if server.Weight != 100 {
		t.Errorf("Expected Weight 100, got %d", server.Weight)
	}

	if server.ActiveConnections != 0 {
		t.Errorf("Expected ActiveConnections 0, got %d", server.ActiveConnections)
	}

	if server.Healthy != true {
		t.Errorf("Expected Healthy true, got %v", server.Healthy)
	}
}

func TestBackendServerFields(t *testing.T) {
	server := &BackendServer{
		ID:                1,
		IP:                "10.0.0.1",
		Port:              9090,
		PortName:          "api",
		Weight:            50,
		ActiveConnections: 5,
		Healthy:           false,
		PreviousHealthy:   true,
	}

	if server.ActiveConnections != 5 {
		t.Errorf("Expected ActiveConnections 5, got %d", server.ActiveConnections)
	}

	if server.Healthy != false {
		t.Errorf("Expected Healthy false, got %v", server.Healthy)
	}

	if server.PreviousHealthy != true {
		t.Errorf("Expected PreviousHealthy true, got %v", server.PreviousHealthy)
	}
}

func TestBackendServerHealthStatus(t *testing.T) {
	server := &BackendServer{
		IP:      "192.168.1.1",
		Port:    8080,
		Healthy: true,
	}

	status := server.healthStatus()
	if status != "healthy" {
		t.Errorf("Expected 'healthy', got '%s'", status)
	}

	server.Healthy = false
	status = server.healthStatus()
	if status != "unhealthy" {
		t.Errorf("Expected 'unhealthy', got '%s'", status)
	}
}

func TestBackendServerHealthCheckWithMockServer(t *testing.T) {
	// Create a test server that responds to connections
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create test listener: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Logf("Warning: Failed to close test listener: %v", err)
		}
	}()

	// Accept connections in a goroutine
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if err := conn.Close(); err != nil {
				log.Printf("Warning: Failed to close test connection: %v", err)
			}
		}
	}()

	// Get the actual port that was assigned
	addr := listener.Addr().(*net.TCPAddr)
	port := addr.Port

	server := &BackendServer{
		IP:      "127.0.0.1",
		Port:    port,
		Healthy: true,
	}

	// Test health check by manually trying connection (since HealthCheck runs in loop)
	connectionTimeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(server.IP, fmt.Sprintf("%d", server.Port)), connectionTimeout)

	if err != nil {
		t.Errorf("Should be able to connect to test server: %v", err)
	} else {
		if err := conn.Close(); err != nil {
			// Only log if it's not an expected "already closed" error
			if !strings.Contains(err.Error(), "use of closed network connection") {
				t.Logf("Warning: Failed to close test connection: %v", err)
			}
		}
	}
}

func TestBackendServerDefaultValues(t *testing.T) {
	server := &BackendServer{}

	if server.ID != 0 {
		t.Errorf("Expected default ID 0, got %d", server.ID)
	}

	if server.IP != "" {
		t.Errorf("Expected default IP empty string, got '%s'", server.IP)
	}

	if server.ActiveConnections != 0 {
		t.Errorf("Expected default ActiveConnections 0, got %d", server.ActiveConnections)
	}

	if server.Healthy != false {
		t.Errorf("Expected default Healthy false, got %v", server.Healthy)
	}
}

func TestBackendServerConnectionManagement(t *testing.T) {
	server := &BackendServer{
		ActiveConnections: 0,
	}

	// Test incrementing connections
	originalCount := server.ActiveConnections
	server.ActiveConnections++

	if server.ActiveConnections != originalCount+1 {
		t.Errorf("Expected ActiveConnections %d, got %d", originalCount+1, server.ActiveConnections)
	}

	// Test decrementing connections
	server.ActiveConnections--

	if server.ActiveConnections != originalCount {
		t.Errorf("Expected ActiveConnections %d, got %d", originalCount, server.ActiveConnections)
	}
}
