package kubernetes

import (
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/cloudresty/nautiluslb/backend"
	"github.com/cloudresty/nautiluslb/config"
)

func TestMatchesLabelSelector(t *testing.T) {
	tests := []struct {
		name           string
		serviceLabels  map[string]string
		labelSelector  string
		expectedResult bool
	}{
		{
			name: "Single label match",
			serviceLabels: map[string]string{
				"app": "nginx",
			},
			labelSelector:  "app=nginx",
			expectedResult: true,
		},
		{
			name: "Single label no match",
			serviceLabels: map[string]string{
				"app": "apache",
			},
			labelSelector:  "app=nginx",
			expectedResult: false,
		},
		{
			name: "Multiple labels match",
			serviceLabels: map[string]string{
				"app.kubernetes.io/name":      "ingress-nginx",
				"app.kubernetes.io/component": "controller",
			},
			labelSelector:  "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller",
			expectedResult: true,
		},
		{
			name: "Multiple labels partial match",
			serviceLabels: map[string]string{
				"app.kubernetes.io/name": "ingress-nginx",
				"version":                "1.0",
			},
			labelSelector:  "app.kubernetes.io/name=ingress-nginx,app.kubernetes.io/component=controller",
			expectedResult: false,
		},
		{
			name:           "Empty service labels",
			serviceLabels:  map[string]string{},
			labelSelector:  "app=nginx",
			expectedResult: false,
		},
		{
			name: "Empty label selector",
			serviceLabels: map[string]string{
				"app": "nginx",
			},
			labelSelector:  "",
			expectedResult: true,
		},
		{
			name: "Label with different value",
			serviceLabels: map[string]string{
				"app": "nginx",
				"env": "prod",
			},
			labelSelector:  "app=nginx,env=dev",
			expectedResult: false,
		},
		{
			name: "Extra service labels should not affect match",
			serviceLabels: map[string]string{
				"app":     "nginx",
				"version": "1.0",
				"team":    "platform",
			},
			labelSelector:  "app=nginx",
			expectedResult: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchesLabelSelector(tt.serviceLabels, tt.labelSelector)
			if result != tt.expectedResult {
				t.Errorf("matchesLabelSelector(%v, %q) = %v; want %v",
					tt.serviceLabels, tt.labelSelector, result, tt.expectedResult)
			}
		})
	}
}

func TestBackendsEqual(t *testing.T) {
	tests := []struct {
		name     string
		old      []*backend.BackendServer
		new      []*backend.BackendServer
		expected bool
	}{
		{
			name:     "Both nil",
			old:      nil,
			new:      nil,
			expected: true,
		},
		{
			name:     "Both empty",
			old:      []*backend.BackendServer{},
			new:      []*backend.BackendServer{},
			expected: true,
		},
		{
			name:     "One nil, one empty",
			old:      nil,
			new:      []*backend.BackendServer{},
			expected: true,
		},
		{
			name: "Different lengths",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080},
				{ID: 2, IP: "192.168.1.2", Port: 8080},
			},
			expected: false,
		},
		{
			name: "Same backends",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.2", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.2", Port: 8080, PortName: "http"},
			},
			expected: true,
		},
		{
			name: "Different IPs",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.2", Port: 8080, PortName: "http"},
			},
			expected: false,
		},
		{
			name: "Different ports",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 9090, PortName: "http"},
			},
			expected: false,
		},
		{
			name: "Different port names",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "https"},
			},
			expected: true, // The current implementation only checks IP:Port, not PortName
		},
		{
			name: "Different order same content",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.2", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 2, IP: "192.168.1.2", Port: 8080, PortName: "http"},
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
			},
			expected: true, // The current implementation is order-independent
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := backendsEqual(tt.old, tt.new)
			if result != tt.expected {
				t.Errorf("backendsEqual() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestProcessServicesForConfig(t *testing.T) {
	// This is a unit test for processServicesForConfig function
	// We'll test it with mock service data

	cfg := config.Configuration{
		Name:            "test-config",
		BackendPortName: "http",
		ListenerAddress: ":80",
	}

	// Test with empty services
	backends := processServicesForConfig(nil, cfg)
	if len(backends) != 0 {
		t.Errorf("Expected 0 backends for nil services, got %d", len(backends))
	}

	// Test with empty slice
	backends = processServicesForConfig([]corev1.Service{}, cfg)
	if len(backends) != 0 {
		t.Errorf("Expected 0 backends for empty services, got %d", len(backends))
	}
}

// Mock LoadBalancer interface for testing
type MockLoadBalancer struct {
	mu             *sync.RWMutex
	backendServers []*backend.BackendServer
}

func (m *MockLoadBalancer) GetMu() *sync.RWMutex {
	if m.mu == nil {
		m.mu = &sync.RWMutex{}
	}
	return m.mu
}

func (m *MockLoadBalancer) GetBackendServers() []*backend.BackendServer {
	if m.backendServers == nil {
		m.backendServers = []*backend.BackendServer{}
	}
	return m.backendServers
}

func (m *MockLoadBalancer) SetBackendServers(servers []*backend.BackendServer) {
	m.backendServers = servers
}

func TestMockLoadBalancer(t *testing.T) {
	// Test our mock implementation
	mock := &MockLoadBalancer{}

	if mock.GetMu() == nil {
		t.Error("GetMu should not return nil")
	}

	servers := mock.GetBackendServers()
	if servers == nil {
		t.Error("GetBackendServers should not return nil")
	}

	if len(servers) != 0 {
		t.Errorf("Expected 0 backend servers initially, got %d", len(servers))
	}

	testServers := []*backend.BackendServer{
		{ID: 1, IP: "192.168.1.1", Port: 8080},
	}

	mock.SetBackendServers(testServers)

	retrievedServers := mock.GetBackendServers()
	if len(retrievedServers) != 1 {
		t.Errorf("Expected 1 backend server, got %d", len(retrievedServers))
	}

	if retrievedServers[0].IP != "192.168.1.1" {
		t.Errorf("Expected IP '192.168.1.1', got '%s'", retrievedServers[0].IP)
	}
}

func TestGetSharedClientError(t *testing.T) {
	// Test error case when no shared client is available
	// This will test the error path since we don't have a real K8s cluster
	sharedK8sClient = nil

	client, err := GetSharedClient()
	if err == nil {
		t.Error("Expected error when no shared client is available")
	}

	if client != nil {
		t.Error("Expected nil client when error occurs")
	}
}
