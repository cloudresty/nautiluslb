package discovery

import (
	"cmp"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudresty/emit"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
)

const (
	// controlPlaneLabel marks control-plane nodes.
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
	// serviceNameLabel ties an EndpointSlice to its Service.
	serviceNameLabel = discoveryv1.LabelServiceName
	// maxWeight bounds the weight annotation.
	maxWeight = 100
)

// splitBindings parses the configurations annotation: comma-separated,
// whitespace-trimmed, empty entries ignored.
func splitBindings(value string) []string {
	var entries []string
	for _, e := range strings.Split(value, ",") {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// namespaceAllowed reports whether spec admits Services of namespace ns
// ("" in spec.Namespaces means every namespace).
func namespaceAllowed(spec config.PoolSpec, ns string) bool {
	return slices.Contains(spec.Namespaces, "") || slices.Contains(spec.Namespaces, ns)
}

// BoundTo reports whether svc is a backend of the pool: it is enabled, names
// the pool's configuration (or "configuration/route" for a tls route pool) in
// its configurations annotation, and lives in a namespace the pool allows. The
// port name alone never binds a Service: any tenant could otherwise name a
// port "https" and receive a share of a public listener's traffic. A plain
// "https" entry does not bind route pools, and "https/web" binds only that
// route's pool.
func BoundTo(svc *corev1.Service, spec config.PoolSpec) bool {
	if svc.Annotations[config.ServiceEnabledAnnotation] != "true" {
		return false
	}
	if !namespaceAllowed(spec, svc.Namespace) {
		return false
	}
	return namesPool(svc, spec)
}

// namesPool reports whether the annotation names the pool, ignoring namespace.
func namesPool(svc *corev1.Service, spec config.PoolSpec) bool {
	for _, entry := range splitBindings(svc.Annotations[config.ServiceConfigurationsAnnotation]) {
		name, route := config.ParseBinding(entry)
		if name == spec.ConfigName && route == spec.Route {
			return true
		}
	}
	return false
}

// nodeSet is the eligible nodes with their dialable addresses.
type nodeSet struct {
	names []string
	addrs map[string]netip.Addr
}

// eligibleNodes applies the node filter and picks one address per node by
// address family (InternalIP preferred over ExternalIP). Nodes without an
// address in an acceptable family are dropped. Sorted by node name.
func eligibleNodes(nodes []*corev1.Node, f config.NodeFilter) nodeSet {
	set := nodeSet{addrs: make(map[string]netip.Addr, len(nodes))}
	for _, n := range nodes {
		if !nodeEligible(n, f) {
			continue
		}
		var cands []netip.Addr
		for _, typ := range []corev1.NodeAddressType{corev1.NodeInternalIP, corev1.NodeExternalIP} {
			for _, a := range n.Status.Addresses {
				if a.Type != typ {
					continue
				}
				if ip, err := netip.ParseAddr(a.Address); err == nil {
					cands = append(cands, ip.Unmap())
				}
			}
		}
		ip, ok := pickFamily(cands, f.AddressFamily)
		if !ok {
			continue
		}
		set.names = append(set.names, n.Name)
		set.addrs[n.Name] = ip
	}
	slices.Sort(set.names)
	return set
}

func nodeEligible(n *corev1.Node, f config.NodeFilter) bool {
	if f.ReadyOnly {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = c.Status == corev1.ConditionTrue
				break
			}
		}
		if !ready {
			return false
		}
	}
	if f.SkipUnschedulable && n.Spec.Unschedulable {
		return false
	}
	if f.SkipControlPlane {
		if _, ok := n.Labels[controlPlaneLabel]; ok {
			return false
		}
	}
	for k, v := range f.Selector {
		if got, ok := n.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// pickFamily returns the first address of the requested family: "ipv4" and
// "ipv6" are strict, "prefer-ipv4" and "prefer-ipv6" fall back to the other
// family. An empty or unknown family means "ipv4".
func pickFamily(addrs []netip.Addr, family string) (netip.Addr, bool) {
	first := func(want4 bool) (netip.Addr, bool) {
		for _, a := range addrs {
			if a.Is4() == want4 {
				return a, true
			}
		}
		return netip.Addr{}, false
	}
	switch family {
	case "ipv6":
		return first(false)
	case "prefer-ipv6":
		if a, ok := first(false); ok {
			return a, true
		}
		return first(true)
	case "prefer-ipv4":
		if a, ok := first(true); ok {
			return a, true
		}
		return first(false)
	default:
		return first(true)
	}
}

// localNodes returns the names of the nodes hosting a ready endpoint of the
// Service, from the Service's EndpointSlices.
func localNodes(svc *corev1.Service, slicesByService map[string][]*discoveryv1.EndpointSlice) map[string]bool {
	out := make(map[string]bool)
	for _, sl := range slicesByService[svc.Namespace+"/"+svc.Name] {
		for _, ep := range sl.Endpoints {
			if ep.NodeName == nil || *ep.NodeName == "" {
				continue
			}
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			out[*ep.NodeName] = true
		}
	}
	return out
}

func indexSlices(slices []*discoveryv1.EndpointSlice) map[string][]*discoveryv1.EndpointSlice {
	idx := make(map[string][]*discoveryv1.EndpointSlice)
	for _, sl := range slices {
		if name := sl.Labels[serviceNameLabel]; name != "" {
			key := sl.Namespace + "/" + name
			idx[key] = append(idx[key], sl)
		}
	}
	return idx
}

// serviceWeight reads the weight annotation: 1..100, default 1; invalid values
// weigh 1 and warn once.
func serviceWeight(svc *corev1.Service, w *warnTracker) int {
	raw, ok := svc.Annotations[config.ServiceWeightAnnotation]
	if !ok {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > maxWeight {
		if w != nil && w.shouldWarn("weight/"+svc.Namespace+"/"+svc.Name, raw) {
			emit.Warn.StructuredFields("Invalid weight annotation, using 1: want an integer in 1..100",
				emit.ZString("namespace", svc.Namespace),
				emit.ZString("service_name", svc.Name),
				emit.ZString("weight_annotation", raw))
		}
		return 1
	}
	return n
}

// portProtocol reports whether a Service port has the protocol the pool needs:
// UDP for udp pools, TCP (or unset) for the rest.
func portProtocol(p corev1.ServicePort, spec config.PoolSpec) bool {
	if spec.Protocol == config.ProtocolUDP {
		return p.Protocol == corev1.ProtocolUDP
	}
	return p.Protocol == "" || p.Protocol == corev1.ProtocolTCP
}

func serviceType(svc *corev1.Service) corev1.ServiceType {
	if svc.Spec.Type == "" {
		return corev1.ServiceTypeClusterIP
	}
	return svc.Spec.Type
}

// needsNodes reports whether the pool has a bound Service that is fed by nodes.
func needsNodes(spec config.PoolSpec, svcs []*corev1.Service) bool {
	for _, svc := range svcs {
		if !BoundTo(svc, spec) {
			continue
		}
		if t := serviceType(svc); t != corev1.ServiceTypeNodePort && t != corev1.ServiceTypeLoadBalancer {
			continue
		}
		for _, p := range svc.Spec.Ports {
			if p.Name == spec.BackendPortName && p.NodePort != 0 && portProtocol(p, spec) {
				return true
			}
		}
	}
	return false
}

// Compute builds the endpoints of every pool from the Services, EndpointSlices
// and Nodes it is given. It is pure and deterministic: the result has an entry
// for every spec (possibly empty), each ordered by namespace, Service, port and
// address. It does not warn; the Manager does, through computeWarn.
func Compute(specs []config.PoolSpec, svcs []*corev1.Service, slices []*discoveryv1.EndpointSlice, nodes []*corev1.Node, f config.NodeFilter) map[string][]backend.Endpoint {
	return computeWarn(specs, svcs, slices, nodes, f, nil)
}

func computeWarn(specs []config.PoolSpec, svcs []*corev1.Service, slices []*discoveryv1.EndpointSlice, nodes []*corev1.Node, f config.NodeFilter, w *warnTracker) map[string][]backend.Endpoint {
	set := eligibleNodes(nodes, f)
	idx := indexSlices(slices)
	out := make(map[string][]backend.Endpoint, len(specs))
	for _, spec := range specs {
		out[spec.Key] = computePool(spec, svcs, idx, set, f, w)
	}
	return out
}

func computePool(spec config.PoolSpec, svcs []*corev1.Service, slicesByService map[string][]*discoveryv1.EndpointSlice, nodes nodeSet, f config.NodeFilter, w *warnTracker) []backend.Endpoint {
	var eps []backend.Endpoint
	for _, svc := range svcs {
		if !BoundTo(svc, spec) {
			continue
		}
		eps = append(eps, serviceEndpoints(svc, spec, slicesByService, nodes, f, w)...)
	}
	slices.SortFunc(eps, func(a, b backend.Endpoint) int {
		return cmp.Or(
			strings.Compare(a.Namespace, b.Namespace),
			strings.Compare(a.Service, b.Service),
			cmp.Compare(a.Address.Port(), b.Address.Port()),
			a.Address.Addr().Compare(b.Address.Addr()),
		)
	})
	return eps
}

func serviceEndpoints(svc *corev1.Service, spec config.PoolSpec, slicesByService map[string][]*discoveryv1.EndpointSlice, nodes nodeSet, f config.NodeFilter, w *warnTracker) []backend.Endpoint {
	var eps []backend.Endpoint
	weight := serviceWeight(svc, w)
	mk := func(ip netip.Addr, port int32, node string) backend.Endpoint {
		return backend.Endpoint{
			Address:   netip.AddrPortFrom(ip, uint16(port)), //nolint:gosec // G115: Kubernetes validates endpoint ports to 1..65535
			Weight:    weight,
			Namespace: svc.Namespace,
			Service:   svc.Name,
			Node:      node,
		}
	}

	switch serviceType(svc) {
	case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		var local map[string]bool
		if svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			local = localNodes(svc, slicesByService)
		}
		for _, p := range svc.Spec.Ports {
			// NodePort is 0 when node port allocation is disabled
			// (allocateLoadBalancerNodePorts=false).
			if p.Name != spec.BackendPortName || p.NodePort <= 0 || p.NodePort > 65535 || !portProtocol(p, spec) {
				continue
			}
			for _, name := range nodes.names {
				if local != nil && !local[name] {
					continue
				}
				eps = append(eps, mk(nodes.addrs[name], p.NodePort, name))
			}
		}

	case corev1.ServiceTypeClusterIP:
		// A headless Service has no virtual IP to dial.
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			break
		}
		ips := svc.Spec.ClusterIPs
		if len(ips) == 0 {
			ips = []string{svc.Spec.ClusterIP}
		}
		var cands []netip.Addr
		for _, s := range ips {
			if ip, err := netip.ParseAddr(s); err == nil {
				cands = append(cands, ip.Unmap())
			}
		}
		ip, ok := pickFamily(cands, f.AddressFamily)
		if !ok {
			break
		}
		for _, p := range svc.Spec.Ports {
			// The ClusterIP listens on port.Port; TargetPort is the pod's.
			if p.Name != spec.BackendPortName || p.Port <= 0 || p.Port > 65535 || !portProtocol(p, spec) {
				continue
			}
			eps = append(eps, mk(ip, p.Port, ""))
		}

	default:
		if w != nil && w.shouldWarn("type/"+svc.Namespace+"/"+svc.Name, string(svc.Spec.Type)) {
			emit.Warn.StructuredFields("Unsupported service type in discovery, ignoring it",
				emit.ZString("service_type", string(svc.Spec.Type)),
				emit.ZString("namespace", svc.Namespace),
				emit.ZString("service_name", svc.Name))
		}
	}
	return eps
}

// warnUnbound warns, once per Service, about enabled Services whose
// annotation entries bind no pool (a typo, or a tls configuration named
// without a route), and about entries naming a pool whose namespaces
// allowlist excludes the Service's namespace: both would otherwise be ignored
// silently.
func warnUnbound(specs []config.PoolSpec, svcs []*corev1.Service, w *warnTracker) {
	if w == nil {
		return
	}
	for _, svc := range svcs {
		if svc.Annotations[config.ServiceEnabledAnnotation] != "true" {
			continue
		}
		value := svc.Annotations[config.ServiceConfigurationsAnnotation]
		entries := splitBindings(value)
		known := false
		for _, entry := range entries {
			name, route := config.ParseBinding(entry)
			matched, allowed := false, false
			for _, spec := range specs {
				if spec.ConfigName == name && spec.Route == route {
					matched = true
					allowed = allowed || namespaceAllowed(spec, svc.Namespace)
				}
			}
			if !matched {
				continue
			}
			known = true
			if !allowed && w.shouldWarn("namespace/"+entry+"/"+svc.Namespace+"/"+svc.Name, "") {
				emit.Warn.StructuredFields("Service names configuration "+entry+" but its namespace is not in that configuration's namespaces allowlist; ignoring",
					emit.ZString("namespace", svc.Namespace),
					emit.ZString("service_name", svc.Name),
					emit.ZString("config_name", entry))
			}
		}
		if known {
			continue
		}
		if w.shouldWarn("binding/"+svc.Namespace+"/"+svc.Name, value) {
			emit.Warn.StructuredFields("Service is enabled but names no known configuration, ignoring it. "+
				"Add the annotation "+config.ServiceConfigurationsAnnotation+"=<configuration name>[/<route>][,...]",
				emit.ZString("namespace", svc.Namespace),
				emit.ZString("service_name", svc.Name),
				emit.ZString("configurations_annotation", value))
		}
	}
}

// endpointsEqual reports whether two endpoint slices hold the same multiset of
// Endpoint values (address, weight and log fields), regardless of order.
func endpointsEqual(a, b []backend.Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[backend.Endpoint]int, len(a))
	for _, e := range a {
		counts[e]++
	}
	for _, e := range b {
		if counts[e] == 0 {
			return false
		}
		counts[e]--
	}
	return true
}
