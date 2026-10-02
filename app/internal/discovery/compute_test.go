package discovery

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
)

const (
	enabledKey = config.ServiceEnabledAnnotation
	configsKey = config.ServiceConfigurationsAnnotation
	weightKey  = config.ServiceWeightAnnotation
)

func node(name string, addrs ...string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, a := range addrs {
		n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: a})
	}
	return n
}

func nodePortSvc(ns, name string, ann map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: ann},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: ports},
	}
}

func bound(configs ...string) map[string]string {
	v := ""
	for i, c := range configs {
		if i > 0 {
			v += ","
		}
		v += c
	}
	return map[string]string{enabledKey: "true", configsKey: v}
}

func poolSpec(name string, namespaces ...string) config.PoolSpec {
	return config.PoolSpec{Key: name, ConfigName: name, BackendPortName: "https", Protocol: config.ProtocolTCP, Namespaces: namespaces}
}

func addrStrings(eps []backend.Endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.Address.String())
	}
	return out
}

func one(spec config.PoolSpec, svcs []*corev1.Service, nodes []*corev1.Node, f config.NodeFilter, sl ...*discoveryv1.EndpointSlice) []string {
	return addrStrings(Compute([]config.PoolSpec{spec}, svcs, sl, nodes, f)[spec.Key])
}

var v4 = config.NodeFilter{AddressFamily: "ipv4"}

func TestBindingMatrix(t *testing.T) {
	cfg := poolSpec("https", "ingress")
	wide := poolSpec("https", "")

	tests := []struct {
		name string
		spec config.PoolSpec
		ns   string
		ann  map[string]string
		want bool
	}{
		{"enabled missing", cfg, "ingress", map[string]string{configsKey: "https"}, false},
		{"enabled false", cfg, "ingress", map[string]string{enabledKey: "false", configsKey: "https"}, false},
		{"configurations missing", cfg, "ingress", map[string]string{enabledKey: "true"}, false},
		{"configurations empty", cfg, "ingress", map[string]string{enabledKey: "true", configsKey: ""}, false},
		{"configurations only commas", cfg, "ingress", map[string]string{enabledKey: "true", configsKey: " , ,"}, false},
		{"other names", cfg, "ingress", bound("http", "mongo"), false},
		{"prefix of name is not a match", cfg, "ingress", bound("https2"), false},
		{"exact name", cfg, "ingress", bound("https"), true},
		{"spaced list", cfg, "ingress", map[string]string{enabledKey: "true", configsKey: " a , https "}, true},
		{"list containing", cfg, "ingress", bound("http", "https", "mongo"), true},
		{"namespace not in allowlist", cfg, "tenant", bound("https"), false},
		{"cluster-wide any namespace", wide, "tenant", bound("https"), true},
		{"cluster-wide still needs name", wide, "tenant", bound("other"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := nodePortSvc(tt.ns, "svc", tt.ann, corev1.ServicePort{Name: "https", NodePort: 30001})
			if got := BoundTo(svc, tt.spec); got != tt.want {
				t.Fatalf("BoundTo = %v, want %v", got, tt.want)
			}
			if got := len(one(tt.spec, []*corev1.Service{svc}, []*corev1.Node{node("n", "10.0.0.1")}, v4)) == 1; got != tt.want {
				t.Fatalf("Compute bound = %v, want %v", got, tt.want)
			}
		})
	}
}

// A tenant must not receive public traffic just by naming a port "https".
func TestTenantCannotHijackListener(t *testing.T) {
	spec := poolSpec("https", "")
	svcs := []*corev1.Service{
		nodePortSvc("ingress", "ingress", bound("https"), corev1.ServicePort{Name: "https", NodePort: 30443}),
		nodePortSvc("tenant", "evil", map[string]string{enabledKey: "true"}, corev1.ServicePort{Name: "https", NodePort: 31111}),
		nodePortSvc("tenant", "evil2", bound("mongo"), corev1.ServicePort{Name: "https", NodePort: 31112}),
		nodePortSvc("tenant", "evil3", map[string]string{configsKey: "https"}, corev1.ServicePort{Name: "https", NodePort: 31113}),
	}
	got := one(spec, svcs, []*corev1.Node{node("n", "10.0.0.1")}, v4)
	if !slices.Equal(got, []string{"10.0.0.1:30443"}) {
		t.Fatalf("endpoints = %v, want only 10.0.0.1:30443", got)
	}
}

func TestServiceTypes(t *testing.T) {
	spec := config.PoolSpec{Key: "web", ConfigName: "web", BackendPortName: "http", Protocol: config.ProtocolTCP, Namespaces: []string{"ns"}}
	svc := func(s corev1.ServiceSpec) []*corev1.Service {
		return []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", Annotations: bound("web")}, Spec: s}}
	}
	nodes := []*corev1.Node{node("n1", "10.0.0.1"), node("n2", "10.0.0.2")}
	tests := []struct {
		name string
		spec corev1.ServiceSpec
		want []string
	}{
		{"ClusterIP uses port not targetPort", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.5",
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}}}, []string{"10.96.0.5:80"}},
		{"headless None skipped", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "None",
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}, nil},
		{"empty ClusterIP skipped", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}, nil},
		{"NodePort zero skipped", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}, nil},
		{"LoadBalancer nodeport used", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 30080}}}, []string{"10.0.0.1:30080", "10.0.0.2:30080"}},
		{"ExternalName skipped", corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}, nil},
		{"other port name skipped", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.5",
			Ports: []corev1.ServicePort{{Name: "metrics", Port: 80}}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := one(spec, svc(tt.spec), nodes, v4); !slices.Equal(got, tt.want) {
				t.Fatalf("endpoints = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProcessServicesForConfigOrderIsDeterministic(t *testing.T) {
	spec := config.PoolSpec{Key: "web", ConfigName: "web", BackendPortName: "http", Protocol: config.ProtocolTCP, Namespaces: []string{""}}
	svcs := []*corev1.Service{
		nodePortSvc("b", "z", bound("web"), corev1.ServicePort{Name: "http", NodePort: 30002}),
		nodePortSvc("a", "y", bound("web"), corev1.ServicePort{Name: "http", NodePort: 30003}),
		nodePortSvc("a", "x", bound("web"), corev1.ServicePort{Name: "http", NodePort: 30009}),
	}
	nodes := []*corev1.Node{node("n2", "10.0.0.2"), node("n1", "10.0.0.1")}
	want := []string{"10.0.0.1:30009", "10.0.0.2:30009", "10.0.0.1:30003", "10.0.0.2:30003", "10.0.0.1:30002", "10.0.0.2:30002"}
	for range 3 {
		if got := one(spec, svcs, nodes, v4); !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		slices.Reverse(svcs)
		slices.Reverse(nodes)
	}
}

func TestRouteBinding(t *testing.T) {
	plain := poolSpec("tcp443", "ns")
	web := config.PoolSpec{Key: "https/web", ConfigName: "https", Route: "web", BackendPortName: "https", Protocol: config.ProtocolTLS, Namespaces: []string{"ns"}}
	api := config.PoolSpec{Key: "https/api", ConfigName: "https", Route: "api", BackendPortName: "https", Protocol: config.ProtocolTLS, Namespaces: []string{"ns"}}
	port := corev1.ServicePort{Name: "https", NodePort: 30001}
	nodes := []*corev1.Node{node("n", "10.0.0.1")}

	tests := []struct {
		name    string
		binding string
		want    map[string]bool
	}{
		{"route entry binds only its route", "https/web", map[string]bool{"https/web": true}},
		{"plain config entry binds no route pool", "https", map[string]bool{}},
		{"both routes", "https/web, https/api", map[string]bool{"https/web": true, "https/api": true}},
		{"route entry does not bind a plain pool", "tcp443/web", map[string]bool{}},
		{"plain pool by plain entry", "tcp443", map[string]bool{"tcp443": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := nodePortSvc("ns", "s", map[string]string{enabledKey: "true", configsKey: tt.binding}, port)
			res := Compute([]config.PoolSpec{plain, web, api}, []*corev1.Service{svc}, nil, nodes, v4)
			for key, eps := range res {
				if got := len(eps) == 1; got != tt.want[key] {
					t.Errorf("pool %s bound = %v, want %v", key, got, tt.want[key])
				}
			}
		})
	}
}

func slice(ns, svc string, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: svc + "-abc", Namespace: ns, Labels: map[string]string{serviceNameLabel: svc}},
		Endpoints:  eps,
	}
}

func epOn(node string, ready *bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{NodeName: &node, Conditions: discoveryv1.EndpointConditions{Ready: ready}}
}

func TestExternalTrafficPolicyLocal(t *testing.T) {
	spec := poolSpec("https", "ns")
	svc := nodePortSvc("ns", "s", bound("https"), corev1.ServicePort{Name: "https", NodePort: 30001})
	svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	nodes := []*corev1.Node{node("n1", "10.0.0.1"), node("n2", "10.0.0.2"), node("n3", "10.0.0.3")}
	yes, no := true, false
	sl := slice("ns", "s", epOn("n2", &yes), epOn("n3", &no), epOn("n1", nil))
	other := slice("ns", "other", epOn("n3", &yes))

	got := one(spec, []*corev1.Service{svc}, nodes, v4, sl, other)
	if want := []string{"10.0.0.1:30001", "10.0.0.2:30001"}; !slices.Equal(got, want) {
		t.Fatalf("Local = %v, want %v (ready-or-unset only, own slices only)", got, want)
	}

	if got := one(spec, []*corev1.Service{svc}, nodes, v4); len(got) != 0 {
		t.Fatalf("Local with no slices = %v, want none", got)
	}

	svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	if got := one(spec, []*corev1.Service{svc}, nodes, v4); len(got) != 3 {
		t.Fatalf("Cluster = %v, want all nodes", got)
	}
}

func TestNodeFilter(t *testing.T) {
	notReady := node("notready", "10.0.0.1")
	notReady.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	ready := func(n *corev1.Node) *corev1.Node {
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		return n
	}
	cordoned := ready(node("cordoned", "10.0.0.2"))
	cordoned.Spec.Unschedulable = true
	cp := ready(node("cp", "10.0.0.3"))
	cp.Labels = map[string]string{controlPlaneLabel: ""}
	worker := ready(node("worker", "10.0.0.4"))
	worker.Labels = map[string]string{"pool": "edge", "zone": "a"}
	other := ready(node("other", "10.0.0.5"))
	other.Labels = map[string]string{"pool": "edge", "zone": "b"}
	all := []*corev1.Node{notReady, cordoned, cp, worker, other}

	spec := poolSpec("https", "ns")
	svc := []*corev1.Service{nodePortSvc("ns", "s", bound("https"), corev1.ServicePort{Name: "https", NodePort: 30001})}
	hosts := func(f config.NodeFilter) []string {
		f.AddressFamily = "ipv4"
		return one(spec, svc, all, f)
	}

	tests := []struct {
		name string
		f    config.NodeFilter
		want []string
	}{
		{"no filter", config.NodeFilter{}, []string{"10.0.0.1:30001", "10.0.0.2:30001", "10.0.0.3:30001", "10.0.0.4:30001", "10.0.0.5:30001"}},
		{"ready only", config.NodeFilter{ReadyOnly: true}, []string{"10.0.0.2:30001", "10.0.0.3:30001", "10.0.0.4:30001", "10.0.0.5:30001"}},
		{"skip unschedulable", config.NodeFilter{SkipUnschedulable: true}, []string{"10.0.0.1:30001", "10.0.0.3:30001", "10.0.0.4:30001", "10.0.0.5:30001"}},
		{"skip control plane", config.NodeFilter{SkipControlPlane: true}, []string{"10.0.0.1:30001", "10.0.0.2:30001", "10.0.0.4:30001", "10.0.0.5:30001"}},
		{"selector", config.NodeFilter{Selector: map[string]string{"pool": "edge"}}, []string{"10.0.0.4:30001", "10.0.0.5:30001"}},
		{"selector is AND", config.NodeFilter{Selector: map[string]string{"pool": "edge", "zone": "a"}}, []string{"10.0.0.4:30001"}},
		{"all filters", config.NodeFilter{ReadyOnly: true, SkipUnschedulable: true, SkipControlPlane: true, Selector: map[string]string{"zone": "b"}}, []string{"10.0.0.5:30001"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hosts(tt.f); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDualStack(t *testing.T) {
	spec := poolSpec("https", "ns")
	nodes := []*corev1.Node{node("dual", "10.0.0.1", "fd00::1"), node("v6only", "fd00::2"), node("v4only", "10.0.0.3")}
	np := []*corev1.Service{nodePortSvc("ns", "s", bound("https"), corev1.ServicePort{Name: "https", NodePort: 30001})}
	cip := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Annotations: bound("https")},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "fd00:10::5",
			ClusterIPs: []string{"fd00:10::5", "10.96.0.5"}, Ports: []corev1.ServicePort{{Name: "https", Port: 443}}},
	}

	tests := []struct {
		family   string
		svcs     []*corev1.Service
		wantNode []string
	}{
		{"ipv4", np, []string{"10.0.0.1:30001", "10.0.0.3:30001"}},
		{"ipv6", np, []string{"[fd00::1]:30001", "[fd00::2]:30001"}},
		{"prefer-ipv4", np, []string{"10.0.0.1:30001", "[fd00::2]:30001", "10.0.0.3:30001"}},
		{"prefer-ipv6", np, []string{"[fd00::1]:30001", "[fd00::2]:30001", "10.0.0.3:30001"}},
		{"ipv4", []*corev1.Service{cip}, []string{"10.96.0.5:443"}},
		{"ipv6", []*corev1.Service{cip}, []string{"[fd00:10::5]:443"}},
		{"prefer-ipv4", []*corev1.Service{cip}, []string{"10.96.0.5:443"}},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d-%s", i, tt.family), func(t *testing.T) {
			got := one(spec, tt.svcs, nodes, config.NodeFilter{AddressFamily: tt.family})
			slices.Sort(got)
			want := slices.Clone(tt.wantNode)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}

	t.Run("strict family with no matching address drops the node", func(t *testing.T) {
		v4cip := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Annotations: bound("https")},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.5",
				Ports: []corev1.ServicePort{{Name: "https", Port: 443}}},
		}
		if got := one(spec, []*corev1.Service{v4cip}, nil, config.NodeFilter{AddressFamily: "ipv6"}); len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})
}

func TestUDPPortProtocol(t *testing.T) {
	tcp := poolSpec("web", "ns")
	udp := config.PoolSpec{Key: "dns", ConfigName: "dns", BackendPortName: "dns", Protocol: config.ProtocolUDP, Namespaces: []string{"ns"}}
	nodes := []*corev1.Node{node("n", "10.0.0.1")}
	svc := nodePortSvc("ns", "s", bound("web", "dns"),
		corev1.ServicePort{Name: "dns", Protocol: corev1.ProtocolUDP, NodePort: 30053},
		corev1.ServicePort{Name: "dns", Protocol: corev1.ProtocolTCP, NodePort: 30054},
		corev1.ServicePort{Name: "https", NodePort: 30443}, // unset protocol means TCP
		corev1.ServicePort{Name: "https", Protocol: corev1.ProtocolUDP, NodePort: 30444},
	)
	res := Compute([]config.PoolSpec{tcp, udp}, []*corev1.Service{svc}, nil, nodes, v4)
	if got := addrStrings(res["dns"]); !slices.Equal(got, []string{"10.0.0.1:30053"}) {
		t.Fatalf("udp pool = %v", got)
	}
	if got := addrStrings(res["web"]); !slices.Equal(got, []string{"10.0.0.1:30443"}) {
		t.Fatalf("tcp pool = %v", got)
	}
}

func TestWeightAnnotation(t *testing.T) {
	spec := poolSpec("https", "ns")
	nodes := []*corev1.Node{node("n", "10.0.0.1")}
	tests := []struct {
		ann  string
		set  bool
		want int
	}{
		{"", false, 1}, {"7", true, 7}, {"1", true, 1}, {"100", true, 100},
		{"0", true, 1}, {"101", true, 1}, {"-3", true, 1}, {"heavy", true, 1}, {"", true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.ann, func(t *testing.T) {
			ann := bound("https")
			if tt.set {
				ann[weightKey] = tt.ann
			}
			svc := nodePortSvc("ns", "s", ann, corev1.ServicePort{Name: "https", NodePort: 30001})
			w := newWarnTracker()
			eps := computeWarn([]config.PoolSpec{spec}, []*corev1.Service{svc}, nil, nodes, v4, w)["https"]
			if len(eps) != 1 || eps[0].Weight != tt.want {
				t.Fatalf("eps = %+v, want weight %d", eps, tt.want)
			}
			if eps[0].Namespace != "ns" || eps[0].Service != "s" || eps[0].Node != "n" {
				t.Fatalf("log fields = %+v", eps[0])
			}
			invalid := tt.set && tt.want == 1 && tt.ann != "1"
			if warned := !w.shouldWarn("weight/ns/s", tt.ann); warned != invalid {
				t.Fatalf("warned = %v, want %v", warned, invalid)
			}
		})
	}
}

func TestWarnTracker(t *testing.T) {
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

func TestWarnTrackerPrunesOnlyOnFullPass(t *testing.T) {
	w := newWarnTracker()
	w.beginPass()
	w.shouldWarn("a", "")
	w.shouldWarn("b", "")
	w.endPass(true)

	w.beginPass()
	w.shouldWarn("a", "")
	w.endPass(false) // incomplete pass: b survives
	if len(w.seen) != 2 {
		t.Fatalf("seen = %v, want 2", w.seen)
	}
	w.beginPass()
	w.shouldWarn("a", "")
	w.endPass(true)
	if _, ok := w.seen["b"]; ok || len(w.seen) != 1 {
		t.Fatalf("seen = %v, want only a", w.seen)
	}
}

func TestUnsupportedTypeWarnsOnce(t *testing.T) {
	spec := poolSpec("web", "ns")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "ns", Annotations: bound("web")},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName},
	}
	w := newWarnTracker()
	computeWarn([]config.PoolSpec{spec}, []*corev1.Service{svc}, nil, nil, v4, w)
	if w.shouldWarn("type/ns/ext", "ExternalName") {
		t.Fatal("unsupported type was not recorded")
	}
}

func TestWarnUnboundOnce(t *testing.T) {
	a := poolSpec("a", "a")
	tlsPool := config.PoolSpec{Key: "https/web", ConfigName: "https", Route: "web", Namespaces: []string{"a"}}
	specs := []config.PoolSpec{a, tlsPool}
	w := newWarnTracker()
	unbound := nodePortSvc("a", "typo", bound("nope"))
	routeless := nodePortSvc("a", "routeless", bound("https")) // names a tls config without a route
	wrongNS := nodePortSvc("b", "svc", bound("a"))
	fine := nodePortSvc("a", "fine", bound("a"))
	disabled := nodePortSvc("a", "off", map[string]string{configsKey: "nope"})

	warnUnbound(specs, []*corev1.Service{unbound, routeless, wrongNS, fine, disabled}, w)

	for _, key := range []string{"binding/a/typo", "binding/a/routeless"} {
		val := map[string]string{"binding/a/typo": "nope", "binding/a/routeless": "https"}[key]
		if w.shouldWarn(key, val) {
			t.Errorf("%s was not warned about", key)
		}
	}
	if w.shouldWarn("namespace/a/b/svc", "") {
		t.Error("namespace allowlist mismatch not warned about")
	}
	if !w.shouldWarn("binding/a/fine", "a") || !w.shouldWarn("binding/a/off", "nope") {
		t.Error("bound or disabled services must not warn")
	}
}

func TestEndpointsEqual(t *testing.T) {
	mk := func(addr string, w int) backend.Endpoint {
		return backend.Endpoint{Address: mustAddrPort(addr), Weight: w}
	}
	a, b := mk("10.0.0.1:80", 1), mk("10.0.0.2:80", 1)
	tests := []struct {
		name string
		x, y []backend.Endpoint
		want bool
	}{
		{"nil and empty", nil, []backend.Endpoint{}, true},
		{"order independent", []backend.Endpoint{a, b}, []backend.Endpoint{b, a}, true},
		{"weight matters", []backend.Endpoint{a}, []backend.Endpoint{mk("10.0.0.1:80", 5)}, false},
		{"length", []backend.Endpoint{a}, []backend.Endpoint{a, b}, false},
		{"multiset", []backend.Endpoint{a, a}, []backend.Endpoint{a, b}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endpointsEqual(tt.x, tt.y); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func mustAddrPort(s string) netip.AddrPort {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		panic(err)
	}
	return ap
}
