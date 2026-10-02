package kubernetes

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
			expected: false, // A renamed port must update: selection filters on PortName
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
	backends := processServicesForConfig(nil, cfg, nil)
	if len(backends) != 0 {
		t.Errorf("Expected 0 backends for nil services, got %d", len(backends))
	}

	// Test with empty slice
	backends = processServicesForConfig([]corev1.Service{}, cfg, nil)
	if len(backends) != 0 {
		t.Errorf("Expected 0 backends for empty services, got %d", len(backends))
	}
}

func TestProcessServicesForConfigNodePort(t *testing.T) {
	cfg := config.Configuration{Name: "https", BackendPortName: "https", ListenerAddress: ":443"}

	services := []corev1.Service{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "ingress",
				Annotations: map[string]string{"nautiluslb.cloudresty.io/enabled": "true"},
			},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{
					{Name: "http", NodePort: 30554},
					{Name: "https", NodePort: 30742},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "not-annotated"},
			Spec: corev1.ServiceSpec{
				Type:  corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{{Name: "https", NodePort: 31000}},
			},
		},
	}

	backends := processServicesForConfig(services, cfg, []string{"10.0.0.1", "10.0.0.2"})
	if len(backends) != 2 {
		t.Fatalf("got %d backends, want 2", len(backends))
	}
	for _, b := range backends {
		if b.Port != 30742 || b.PortName != "https" || !b.IsHealthy() {
			t.Errorf("unexpected backend %s %s healthy=%v", b.Address(), b.PortName, b.IsHealthy())
		}
	}
}

// fakeLB records SetBackendServers calls.
type fakeLB struct {
	mu      sync.Mutex
	servers []*backend.BackendServer
	sets    int
}

func (f *fakeLB) GetBackendServers() []*backend.BackendServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*backend.BackendServer{}, f.servers...)
}

func (f *fakeLB) SetBackendServers(servers []*backend.BackendServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = servers
	f.sets++
}

func ingressFixtures() []runtime.Object {
	return []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
			}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "ingress",
				Namespace:   "ingress",
				Annotations: map[string]string{"nautiluslb.cloudresty.io/enabled": "true"},
			},
			Spec: corev1.ServiceSpec{
				Type:  corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{{Name: "https", NodePort: 30742}},
			},
		},
	}
}

func TestDiscoverOnceUpdatesBackends(t *testing.T) {
	client := fake.NewClientset(ingressFixtures()...)
	lb := &fakeLB{}
	cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespace: "ingress"}

	discoverOnce(context.Background(), client,
		map[string][]config.Configuration{"ingress": {cfg}},
		map[string]LoadBalancerInterface{"https": lb})

	servers := lb.GetBackendServers()
	if len(servers) != 1 || servers[0].Address() != "10.0.0.1:30742" {
		t.Fatalf("unexpected backends: %d", len(servers))
	}
}

// An API failure (the production case was an expired client certificate,
// "Unauthorized") must keep the backends already known, never wipe them.
func TestDiscoverOnceKeepsBackendsWhenTheAPIFails(t *testing.T) {
	for _, resource := range []string{"nodes", "services"} {
		t.Run(resource, func(t *testing.T) {
			client := fake.NewClientset(ingressFixtures()...)
			client.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("Unauthorized")
			})

			known := backend.New(1, "10.0.0.9", 30742, "https")
			lb := &fakeLB{servers: []*backend.BackendServer{known}}
			cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespace: "ingress"}

			discoverOnce(context.Background(), client,
				map[string][]config.Configuration{"ingress": {cfg}},
				map[string]LoadBalancerInterface{"https": lb})

			if lb.sets != 0 {
				t.Fatalf("backends replaced %d times after a failed %s list", lb.sets, resource)
			}
			if servers := lb.GetBackendServers(); len(servers) != 1 || servers[0] != known {
				t.Fatal("known backends were lost")
			}
		})
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
