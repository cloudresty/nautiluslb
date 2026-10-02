package discovery

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
)

func TestLegacyBackendsEqual(t *testing.T) {
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
		{
			name: "Symmetric: old has a key new lacks",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.2", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.3", Port: 8080, PortName: "http"},
			},
			expected: false,
		},
		{
			name: "Duplicates are a multiset",
			old: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.1", Port: 8080, PortName: "http"},
			},
			new: []*backend.BackendServer{
				{ID: 1, IP: "192.168.1.1", Port: 8080, PortName: "http"},
				{ID: 2, IP: "192.168.1.2", Port: 8080, PortName: "http"},
			},
			expected: false,
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

const (
	legacyEnabledKey = config.ServiceEnabledAnnotation
	legacyConfigsKey = config.ServiceConfigurationsAnnotation
)

func legacyNodeObj(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: ip},
		}},
	}
}

func legacyNodePortSvc(ns, name string, annotations map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: ports},
	}
}

func legacyAddrs(servers []*backend.BackendServer) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Address())
	}
	return out
}

func TestLegacyBindingMatrix(t *testing.T) {
	cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespace: "ingress"}
	clusterWide := config.Configuration{Name: "https", BackendPortName: "https", Namespaces: []string{"*"}}

	tests := []struct {
		name        string
		cfg         config.Configuration
		namespace   string
		annotations map[string]string
		want        bool
	}{
		{"enabled missing", cfg, "ingress", map[string]string{legacyConfigsKey: "https"}, false},
		{"enabled false", cfg, "ingress", map[string]string{legacyEnabledKey: "false", legacyConfigsKey: "https"}, false},
		{"configurations missing", cfg, "ingress", map[string]string{legacyEnabledKey: "true"}, false},
		{"configurations empty", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: ""}, false},
		{"configurations only commas", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: " , ,"}, false},
		{"other names", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "http,mongo"}, false},
		{"prefix of name is not a match", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https2"}, false},
		{"exact name", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"}, true},
		{"spaced list", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: " a , https "}, true},
		{"list containing", cfg, "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "http,https,mongo"}, true},
		{"namespace not in allowlist", cfg, "tenant", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"}, false},
		{"cluster-wide any namespace", clusterWide, "tenant", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"}, true},
		{"cluster-wide still needs name", clusterWide, "tenant", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "other"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := legacyNodePortSvc(tt.namespace, "svc", tt.annotations, corev1.ServicePort{Name: "https", NodePort: 30001})
			backends := processServicesForConfig([]corev1.Service{*svc}, tt.cfg, []string{"10.0.0.1"}, newWarnTracker())
			if got := len(backends) == 1; got != tt.want {
				t.Fatalf("bound = %v, want %v", got, tt.want)
			}
		})
	}
}

// A tenant must not receive public traffic just by naming a port "https".
func TestLegacyTenantCannotHijackListener(t *testing.T) {
	cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespaces: []string{"*"}}
	ok := map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"}
	tenant := map[string]string{legacyEnabledKey: "true"}

	services := []corev1.Service{
		*legacyNodePortSvc("ingress", "ingress", ok, corev1.ServicePort{Name: "https", NodePort: 30443}),
		*legacyNodePortSvc("tenant", "evil", tenant, corev1.ServicePort{Name: "https", NodePort: 31111}),
		*legacyNodePortSvc("tenant", "evil2", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "mongo"}, corev1.ServicePort{Name: "https", NodePort: 31112}),
	}

	backends := processServicesForConfig(services, cfg, []string{"10.0.0.1"}, newWarnTracker())
	if got := legacyAddrs(backends); len(got) != 1 || got[0] != "10.0.0.1:30443" {
		t.Fatalf("backends = %v, want only 10.0.0.1:30443", got)
	}
}

func TestLegacyServiceTypes(t *testing.T) {
	cfg := config.Configuration{Name: "web", BackendPortName: "http", Namespace: "ns"}
	ann := map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "web"}
	svc := func(spec corev1.ServiceSpec) []corev1.Service {
		return []corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", Annotations: ann}, Spec: spec}}
	}

	tests := []struct {
		name string
		spec corev1.ServiceSpec
		want []string
	}{
		{"ClusterIP uses port not targetPort", corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.5",
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}},
		}, []string{"10.96.0.5:80"}},
		{"headless None skipped", corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "None",
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
		}, nil},
		{"empty ClusterIP skipped", corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
		}, nil},
		{"NodePort zero skipped", corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 0}},
		}, nil},
		{"LoadBalancer nodeport used", corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 30080}},
		}, []string{"10.0.0.1:30080", "10.0.0.2:30080"}},
		{"ExternalName skipped", corev1.ServiceSpec{
			Type:  corev1.ServiceTypeExternalName,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
		}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := legacyAddrs(processServicesForConfig(svc(tt.spec), cfg, []string{"10.0.0.1", "10.0.0.2"}, newWarnTracker()))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("backends = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLegacyProcessServicesForConfigOrderIsDeterministic(t *testing.T) {
	cfg := config.Configuration{Name: "web", BackendPortName: "http", Namespaces: []string{"*"}}
	ann := map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "web"}
	services := []corev1.Service{
		*legacyNodePortSvc("b", "z", ann, corev1.ServicePort{Name: "http", NodePort: 30002}),
		*legacyNodePortSvc("a", "y", ann, corev1.ServicePort{Name: "http", NodePort: 30003}),
		*legacyNodePortSvc("a", "x", ann, corev1.ServicePort{Name: "http", NodePort: 30009}),
	}
	nodes := []string{"10.0.0.2", "10.0.0.1"}

	want := []string{"10.0.0.1:30009", "10.0.0.2:30009", "10.0.0.1:30003", "10.0.0.2:30003", "10.0.0.1:30002", "10.0.0.2:30002"}

	slices.Reverse(services)
	for range 2 {
		backends := processServicesForConfig(services, cfg, nodes, newWarnTracker())
		if got := legacyAddrs(backends); !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		for i, b := range backends {
			if b.ID != i+1 {
				t.Fatalf("ID[%d] = %d", i, b.ID)
			}
		}
		slices.Reverse(services)
	}
}

func TestLegacyWarnTracker(t *testing.T) {
	w := newWarnTracker()
	if !w.shouldWarn("binding/ns/svc", "") {
		t.Fatal("first warning suppressed")
	}
	if w.shouldWarn("binding/ns/svc", "") {
		t.Fatal("repeat warning not deduplicated")
	}
	if !w.shouldWarn("binding/ns/svc", "typo") {
		t.Fatal("changed value should warn again")
	}
	if !w.shouldWarn("binding/ns/other", "") {
		t.Fatal("other service should warn")
	}
}

func TestLegacyUnsupportedTypeWarnsOnce(t *testing.T) {
	cfg := config.Configuration{Name: "web", BackendPortName: "http", Namespace: "ns"}
	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "ns", Annotations: map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "web"}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName},
	}
	w := newWarnTracker()
	processServicesForConfig([]corev1.Service{svc}, cfg, nil, w)
	if w.shouldWarn("type/ns/ext", "ExternalName") {
		t.Fatal("unsupported type was not recorded on first pass")
	}
}

// legacyFakeLB records SetBackendServers calls.
type legacyFakeLB struct {
	mu      sync.Mutex
	servers []*backend.BackendServer
	sets    int
}

func (f *legacyFakeLB) GetBackendServers() []*backend.BackendServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*backend.BackendServer{}, f.servers...)
}

func (f *legacyFakeLB) SetBackendServers(servers []*backend.BackendServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = servers
	f.sets++
}

func (f *legacyFakeLB) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets
}

func legacyLbFor(cfgs ...config.Configuration) map[string]LoadBalancerInterface {
	m := make(map[string]LoadBalancerInterface, len(cfgs))
	for _, c := range cfgs {
		m[c.Name] = &legacyFakeLB{}
	}
	return m
}

func TestLegacyDiscoverOnceUpdatesBackends(t *testing.T) {
	cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespace: "ingress"}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("ingress", "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"},
			corev1.ServicePort{Name: "https", NodePort: 30742}))
	lbs := legacyLbFor(cfg)

	discoverOnce(context.Background(), client, []config.Configuration{cfg}, lbs, newWarnTracker())

	servers := lbs["https"].GetBackendServers()
	if len(servers) != 1 || servers[0].Address() != "10.0.0.1:30742" {
		t.Fatalf("unexpected backends: %v", legacyAddrs(servers))
	}
}

func TestLegacyDiscoverOnceAggregatesNamespacesInOneCall(t *testing.T) {
	cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespaces: []string{"a", "b"}}
	ann := map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("a", "svc", ann, corev1.ServicePort{Name: "https", NodePort: 30001}),
		legacyNodePortSvc("b", "svc", ann, corev1.ServicePort{Name: "https", NodePort: 30002}),
		legacyNodePortSvc("c", "svc", ann, corev1.ServicePort{Name: "https", NodePort: 30003}),
	)
	lbs := legacyLbFor(cfg)

	discoverOnce(context.Background(), client, []config.Configuration{cfg}, lbs, newWarnTracker())

	lb := lbs["https"].(*legacyFakeLB)
	if lb.setCount() != 1 {
		t.Fatalf("SetBackendServers called %d times, want 1", lb.setCount())
	}
	if got, want := legacyAddrs(lb.GetBackendServers()), []string{"10.0.0.1:30001", "10.0.0.1:30002"}; !slices.Equal(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}

	// An identical second pass must not call SetBackendServers again.
	discoverOnce(context.Background(), client, []config.Configuration{cfg}, lbs, newWarnTracker())
	if lb.setCount() != 1 {
		t.Fatalf("unchanged pass called SetBackendServers (%d)", lb.setCount())
	}
}

func TestLegacyDiscoverOnceClusterWideAndScopedUseOwnLists(t *testing.T) {
	wide := config.Configuration{Name: "wide", BackendPortName: "https", Namespaces: []string{"*"}}
	scoped := config.Configuration{Name: "scoped", BackendPortName: "https", Namespace: "a"}
	ann := map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "wide,scoped"}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("a", "svc", ann, corev1.ServicePort{Name: "https", NodePort: 30001}),
		legacyNodePortSvc("b", "svc", ann, corev1.ServicePort{Name: "https", NodePort: 30002}),
	)
	lbs := legacyLbFor(wide, scoped)

	discoverOnce(context.Background(), client, []config.Configuration{wide, scoped}, lbs, newWarnTracker())

	if got := legacyAddrs(lbs["wide"].GetBackendServers()); len(got) != 2 {
		t.Fatalf("wide backends = %v", got)
	}
	if got := legacyAddrs(lbs["scoped"].GetBackendServers()); !slices.Equal(got, []string{"10.0.0.1:30001"}) {
		t.Fatalf("scoped backends = %v", got)
	}
}

// A failing cluster-wide list (no cluster-scope RBAC) must not freeze a
// scoped configuration, and leaves the cluster-wide one untouched.
func TestLegacyDiscoverOnceClusterWideListFailureDoesNotFreezeScoped(t *testing.T) {
	wide := config.Configuration{Name: "wide", BackendPortName: "https", Namespaces: []string{"*"}}
	scoped := config.Configuration{Name: "scoped", BackendPortName: "https", Namespace: "a"}
	ann := map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "wide,scoped"}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("a", "svc", ann, corev1.ServicePort{Name: "https", NodePort: 30001}))
	client.PrependReactor("list", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == metav1.NamespaceAll {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	lbs := legacyLbFor(wide, scoped)

	discoverOnce(context.Background(), client, []config.Configuration{wide, scoped}, lbs, newWarnTracker())

	if got := legacyAddrs(lbs["scoped"].GetBackendServers()); !slices.Equal(got, []string{"10.0.0.1:30001"}) {
		t.Fatalf("scoped backends = %v", got)
	}
	if n := lbs["wide"].(*legacyFakeLB).setCount(); n != 0 {
		t.Fatalf("wide config updated %d times despite failed list", n)
	}
}

func TestLegacyWarnTrackerPrunesGoneServices(t *testing.T) {
	cfg := config.Configuration{Name: "web", BackendPortName: "http", Namespace: "ns"}
	ext := func(name string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Annotations: map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "web"}},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName},
		}
	}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"), ext("keep"), ext("gone"))
	lbs := legacyLbFor(cfg)
	w := newWarnTracker()
	run := func() { discoverOnce(context.Background(), client, []config.Configuration{cfg}, lbs, w) }

	run()
	if len(w.seen) != 2 {
		t.Fatalf("seen = %v, want 2 keys", w.seen)
	}

	// A failed pass must not prune.
	if err := client.CoreV1().Services("ns").Delete(context.Background(), "gone", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	run()
	if len(w.seen) != 2 {
		t.Fatalf("failed pass pruned: seen = %v", w.seen)
	}

	// A successful pass prunes the vanished Service only.
	client.ReactionChain = client.ReactionChain[1:]
	run()
	if _, ok := w.seen["type/ns/gone"]; ok || len(w.seen) != 1 {
		t.Fatalf("seen = %v, want only type/ns/keep", w.seen)
	}
	if w.shouldWarn("type/ns/keep", "ExternalName") {
		t.Fatal("still-present Service was re-warned")
	}
}

func TestLegacyWarnsWhenNamespaceNotInAllowlist(t *testing.T) {
	a := config.Configuration{Name: "a", BackendPortName: "https", Namespace: "a"}
	b := config.Configuration{Name: "b", BackendPortName: "https", Namespace: "b"}
	// Listed for b, but names a, whose allowlist excludes namespace b.
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("b", "svc", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "a"}, corev1.ServicePort{Name: "https", NodePort: 30001}))
	w := newWarnTracker()

	discoverOnce(context.Background(), client, []config.Configuration{a, b}, legacyLbFor(a, b), w)

	if w.shouldWarn("namespace/a/b/svc", "") {
		t.Fatal("allowlist mismatch was not warned about")
	}
}

// A failed list leaves the configurations that need it untouched, without
// blocking the others.
func TestLegacyDiscoverOnceFailedListLeavesOnlyAffectedConfig(t *testing.T) {
	multi := config.Configuration{Name: "multi", BackendPortName: "https", Namespaces: []string{"a", "b"}}
	other := config.Configuration{Name: "other", BackendPortName: "https", Namespace: "c"}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("a", "svc", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "multi"}, corev1.ServicePort{Name: "https", NodePort: 30001}),
		legacyNodePortSvc("c", "svc", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "other"}, corev1.ServicePort{Name: "https", NodePort: 30003}),
	)
	client.PrependReactor("list", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "b" {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})

	known := backend.NewServer(1, "10.0.0.9", 30009, "https")
	lbs := legacyLbFor(multi, other)
	lbs["multi"].(*legacyFakeLB).servers = []*backend.BackendServer{known}

	discoverOnce(context.Background(), client, []config.Configuration{multi, other}, lbs, newWarnTracker())

	if lb := lbs["multi"].(*legacyFakeLB); lb.setCount() != 0 || lb.GetBackendServers()[0] != known {
		t.Fatal("multi-namespace config was updated from a partial result")
	}
	if got := legacyAddrs(lbs["other"].GetBackendServers()); !slices.Equal(got, []string{"10.0.0.1:30003"}) {
		t.Fatalf("other backends = %v", got)
	}
}

// An API failure (the production case was an expired client certificate,
// "Unauthorized") must keep the backends already known, never wipe them.
func TestLegacyDiscoverOnceKeepsBackendsWhenTheAPIFails(t *testing.T) {
	for _, resource := range []string{"nodes", "services"} {
		t.Run(resource, func(t *testing.T) {
			cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespace: "ingress"}
			client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
				legacyNodePortSvc("ingress", "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"},
					corev1.ServicePort{Name: "https", NodePort: 30742}))
			client.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("Unauthorized")
			})

			known := backend.NewServer(1, "10.0.0.9", 30742, "https")
			lb := &legacyFakeLB{servers: []*backend.BackendServer{known}}

			discoverOnce(context.Background(), client, []config.Configuration{cfg},
				map[string]LoadBalancerInterface{"https": lb}, newWarnTracker())

			if n := lb.setCount(); n != 0 {
				t.Fatalf("backends replaced %d times after a failed %s list", n, resource)
			}
			if servers := lb.GetBackendServers(); len(servers) != 1 || servers[0] != known {
				t.Fatal("known backends were lost")
			}
		})
	}
}

func TestLegacyRunDiscoveryStopsOnCancel(t *testing.T) {
	cfg := config.Configuration{Name: "https", BackendPortName: "https", Namespace: "ingress"}
	client := fake.NewClientset(legacyNodeObj("n1", "10.0.0.1"),
		legacyNodePortSvc("ingress", "ingress", map[string]string{legacyEnabledKey: "true", legacyConfigsKey: "https"},
			corev1.ServicePort{Name: "https", NodePort: 30742}))
	lbs := legacyLbFor(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDiscovery(ctx, client, []config.Configuration{cfg}, lbs, 10*time.Millisecond)
	}()

	// The first pass runs immediately, well before the first tick.
	deadline := time.Now().Add(5 * time.Second)
	for lbs["https"].(*legacyFakeLB).setCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first pass never ran")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not exit on cancel")
	}
}
func TestLegacyGetSharedClientError(t *testing.T) {
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
