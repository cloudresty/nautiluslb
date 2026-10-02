package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
)

// Clientset is an alias for kubernetes.Clientset
type Clientset = kubernetes.Clientset

var (
	sharedK8sClient *kubernetes.Clientset
)

// LoadBalancerInterface defines the methods discovery needs from a
// LoadBalancer. Both must be safe for concurrent use: discovery runs in its
// own goroutine while connections are being served.
type LoadBalancerInterface interface {
	GetBackendServers() []*backend.BackendServer
	SetBackendServers(servers []*backend.BackendServer)
}

// GetSharedClient returns the shared Kubernetes client.
// It returns an error if the client has not been initialized yet.
func GetSharedClient() (*kubernetes.Clientset, error) {
	if sharedK8sClient == nil {
		return nil, fmt.Errorf("shared Kubernetes client not initialized. " +
			"Call GetK8sClient in main.go to initialize the client before using it in other functions")
	}
	return sharedK8sClient, nil
}

// GetK8sClient initializes and returns a Kubernetes client and the current context.
func GetK8sClient(kubeconfigPath string) (*kubernetes.Clientset, string, error) {

	var config *rest.Config
	var currentContext string

	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {

		emit.Info.Msg("Using in-cluster Kubernetes config")
		currentContext = "in-cluster"

	} else {

		emit.Debug.StructuredFields("Failed to get in-cluster config",
			emit.ZString("error", err.Error()))

		// Fallback to kubeconfig file
		if kubeconfigPath == "" {

			emit.Debug.Msg("KUBECONFIG environment variable not set, using default ~/.kube/config")

			home, err := os.UserHomeDir()
			if err != nil {
				return nil, "", fmt.Errorf("failed to get user home directory: %v", err)
			}

			kubeconfigPath = filepath.Join(home, ".kube", "config")

		} else {

			emit.Debug.StructuredFields("Using KUBECONFIG",
				emit.ZString("kubeconfig_path", kubeconfigPath))

		}

		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to get Kubernetes config from %s: %v", kubeconfigPath, err)
		}

		// Get the current context from the kubeconfig file
		kubeconfig, err := clientcmd.LoadFromFile(kubeconfigPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to load kubeconfig file: %v", err)
		}

		currentContext = kubeconfig.CurrentContext

	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	sharedK8sClient = clientset // Store the client in the package-level variable
	return sharedK8sClient, currentContext, nil

}

// discoveryInterval is the interval between service discovery passes.
const discoveryInterval = 30 * time.Second

// apiTimeout bounds one Kubernetes API call. client-go applies no timeout of
// its own, so a hung API server would otherwise stop discovery for good.
const apiTimeout = 30 * time.Second

func getNodeIPs(ctx context.Context, k8sClient kubernetes.Interface) ([]string, error) {

	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	var ips []string

	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ips = append(ips, addr.Address)
				break
			}
		}
	}

	return ips, nil

}

// warnTracker remembers what has been warned about, so a misconfigured
// Service is reported once rather than on every 30s pass. A changed value
// (the annotation was edited, still wrongly) warns again.
//
// Keys touched during a pass are remembered so endPass can drop the ones whose
// Service is gone, keeping the tracker bounded under Service churn.
type warnTracker struct {
	mu      sync.Mutex
	seen    map[string]string
	touched map[string]struct{}
}

func newWarnTracker() *warnTracker {
	return &warnTracker{seen: make(map[string]string), touched: make(map[string]struct{})}
}

// beginPass starts tracking which keys a discovery pass touches.
func (w *warnTracker) beginPass() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.touched = make(map[string]struct{})
}

// endPass finishes a pass. With prune set, keys not touched during the pass are
// dropped. A pass that could not list every Service must not prune: the
// Services it missed would warn again on the next pass.
func (w *warnTracker) endPass(prune bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if prune {
		for key := range w.seen {
			if _, ok := w.touched[key]; !ok {
				delete(w.seen, key)
			}
		}
	}
	w.touched = make(map[string]struct{})
}

// shouldWarn reports whether key has not yet been warned about with value,
// and records it.
func (w *warnTracker) shouldWarn(key, value string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.touched[key] = struct{}{}
	if old, ok := w.seen[key]; ok && old == value {
		return false
	}
	w.seen[key] = value
	return true
}

// DiscoverK8sServicesForAll discovers services for all load balancers
// centrally, until ctx is cancelled. The first pass runs immediately.
func DiscoverK8sServicesForAll(ctx context.Context, loadBalancers []LoadBalancerInterface, configs []config.Configuration) {

	emit.Info.Msg("Starting centralized service discovery for all load balancers")

	// Get the shared Kubernetes client
	k8sClient, err := GetSharedClient()
	if err != nil {
		emit.Error.StructuredFields("Failed to get K8s client in centralized discovery",
			emit.ZString("error", err.Error()))
		return
	}

	// Create a map of config name to load balancer for quick lookup
	configToLB := make(map[string]LoadBalancerInterface)
	for i, cfg := range configs {
		if i < len(loadBalancers) {
			configToLB[cfg.Name] = loadBalancers[i]
		}
	}

	runDiscovery(ctx, k8sClient, configs, configToLB, discoveryInterval)
}

// runDiscovery runs a pass immediately and then every interval until ctx is
// cancelled.
func runDiscovery(ctx context.Context, k8sClient kubernetes.Interface, configs []config.Configuration, configToLB map[string]LoadBalancerInterface, interval time.Duration) {

	warned := newWarnTracker()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {

		discoverOnce(ctx, k8sClient, configs, configToLB, warned)

		select {
		case <-ctx.Done():
			emit.Info.Msg("Service discovery stopped")
			return
		case <-ticker.C:
		}

	}

}

// listResult is the outcome of listing the Services of one namespace.
type listResult struct {
	services []corev1.Service
	err      error
}

// discoverOnce runs one discovery pass. Node IPs are listed once per pass.
// If that fails the pass is skipped: an empty node list would otherwise
// replace every NodePort backend with nothing, turning an API blip (or an
// expired credential) into an outage.
//
// Services are listed once per distinct namespace any configuration needs
// (the cluster-wide list is fetched only for cluster-wide configurations, so a
// scoped configuration never depends on cluster-scope RBAC). Each
// configuration then gets the backends from ALL its namespaces in a single
// SetBackendServers call, and is left untouched if any list it needs failed:
// a partial result would silently drop the other namespaces' backends.
func discoverOnce(ctx context.Context, k8sClient kubernetes.Interface, configs []config.Configuration, configToLB map[string]LoadBalancerInterface, warned *warnTracker) {

	nodeIPs, err := getNodeIPs(ctx, k8sClient)
	if err != nil {
		if ctx.Err() == nil {
			emit.Error.StructuredFields("Failed to list nodes, keeping current backends",
				emit.ZString("error", err.Error()))
		}
		return
	}

	warned.beginPass()

	// Distinct namespaces to list.
	var namespaces []string
	for _, cfg := range configs {
		for _, ns := range cfg.DiscoveryNamespaces() {
			if !slices.Contains(namespaces, ns) {
				namespaces = append(namespaces, ns)
			}
		}
	}

	allListed := true
	lists := make(map[string]listResult, len(namespaces))
	for _, ns := range namespaces {
		services, err := listServices(ctx, k8sClient, ns)
		if err != nil {
			allListed = false
			if ctx.Err() != nil {
				return
			}
			emit.Error.StructuredFields("Failed to list services in centralized discovery, keeping current backends of the configurations that need them",
				emit.ZString("namespace", ns),
				emit.ZString("error", err.Error()))
		}
		lists[ns] = listResult{services: services, err: err}
	}

	warnUnbound(lists, configs, warned)
	defer func() { warned.endPass(allListed) }()

	for _, cfg := range configs {

		lb, exists := configToLB[cfg.Name]
		if !exists {
			continue
		}

		services, ok := servicesFor(lists, cfg.DiscoveryNamespaces())
		if !ok {
			continue
		}

		backends := processServicesForConfig(services, cfg, nodeIPs, warned)
		currentBackends := lb.GetBackendServers()

		// Only update if backends changed. SetBackendServers also
		// starts and stops the matching health checks.
		if !backendsEqual(currentBackends, backends) {
			lb.SetBackendServers(backends)
			emit.Info.StructuredFields("Updated backends for config",
				emit.ZInt("backend_count", len(backends)),
				emit.ZString("config_name", cfg.Name))
		}

	}

}

func listServices(ctx context.Context, k8sClient kubernetes.Interface, namespace string) ([]corev1.Service, error) {

	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	list, err := k8sClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services in namespace %q: %w", namespace, err)
	}

	return list.Items, nil

}

// servicesFor gathers the Services of the given namespaces from lists. It
// reports false if any list needed failed. A configuration only ever uses the
// lists of its own namespaces.
func servicesFor(lists map[string]listResult, namespaces []string) ([]corev1.Service, bool) {

	var services []corev1.Service
	for _, ns := range namespaces {
		res, ok := lists[ns]
		if !ok || res.err != nil {
			return nil, false
		}
		services = append(services, res.services...)
	}

	return services, true

}

// splitConfigurations parses the configurations annotation: comma-separated,
// whitespace-trimmed, empty entries ignored.
func splitConfigurations(value string) []string {

	var names []string
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}

	return names

}

// warnUnbound warns, once per Service, about enabled Services that name none
// of the known configurations, or that name a known configuration whose
// namespaces allowlist excludes the Service's namespace: they would otherwise
// be ignored silently. The second case is only observable when the Service was
// listed for another configuration.
func warnUnbound(lists map[string]listResult, configs []config.Configuration, warned *warnTracker) {

	known := make(map[string]bool, len(configs))
	byName := make(map[string]config.Configuration, len(configs))
	for _, cfg := range configs {
		known[cfg.Name] = true
		byName[cfg.Name] = cfg
	}

	for _, res := range lists {
		for _, service := range res.services {

			if service.Annotations[config.ServiceEnabledAnnotation] != "true" {
				continue
			}

			value := service.Annotations[config.ServiceConfigurationsAnnotation]

			for _, name := range splitConfigurations(value) {
				cfg, ok := byName[name]
				if !ok {
					continue
				}
				namespaces := cfg.DiscoveryNamespaces()
				if slices.Contains(namespaces, metav1.NamespaceAll) || slices.Contains(namespaces, service.Namespace) {
					continue
				}
				if warned.shouldWarn("namespace/"+name+"/"+service.Namespace+"/"+service.Name, "") {
					emit.Warn.StructuredFields("Service names configuration "+name+" but its namespace is not in that configuration's namespaces allowlist; ignoring",
						emit.ZString("namespace", service.Namespace),
						emit.ZString("service_name", service.Name),
						emit.ZString("config_name", name))
				}
			}

			if slices.ContainsFunc(splitConfigurations(value), func(name string) bool { return known[name] }) {
				continue
			}

			if warned.shouldWarn("binding/"+service.Namespace+"/"+service.Name, value) {
				emit.Warn.StructuredFields("Service is enabled but names no known configuration, ignoring it. "+
					"Add the annotation "+config.ServiceConfigurationsAnnotation+"=<configuration name>[,<name>...]",
					emit.ZString("namespace", service.Namespace),
					emit.ZString("service_name", service.Name),
					emit.ZString("configurations_annotation", value))
			}

		}
	}

}

// boundTo reports whether service is a backend of cfg: it is enabled, names
// cfg in its configurations annotation, and lives in a namespace cfg allows.
// The port name alone never binds a Service: any tenant could otherwise name a
// port "https" and receive a share of the public listener's traffic.
func boundTo(service corev1.Service, cfg config.Configuration) bool {

	if service.Annotations[config.ServiceEnabledAnnotation] != "true" {
		return false
	}

	if !slices.Contains(splitConfigurations(service.Annotations[config.ServiceConfigurationsAnnotation]), cfg.Name) {
		return false
	}

	namespaces := cfg.DiscoveryNamespaces()
	return slices.Contains(namespaces, metav1.NamespaceAll) || slices.Contains(namespaces, service.Namespace)

}

// processServicesForConfig builds the backends of one configuration from
// services, in a deterministic order.
func processServicesForConfig(services []corev1.Service, cfg config.Configuration, nodeIPs []string, warned *warnTracker) []*backend.BackendServer {

	type entry struct {
		namespace, service string
		server             *backend.BackendServer
	}

	var entries []entry

	for _, service := range services {

		if !boundTo(service, cfg) {
			continue
		}

		for _, server := range processServiceForConfig(service, cfg, nodeIPs, warned) {
			entries = append(entries, entry{service.Namespace, service.Name, server})
		}

	}

	slices.SortFunc(entries, func(a, b entry) int {
		if c := strings.Compare(a.namespace, b.namespace); c != 0 {
			return c
		}
		if c := strings.Compare(a.service, b.service); c != 0 {
			return c
		}
		if a.server.Port != b.server.Port {
			return a.server.Port - b.server.Port
		}
		return strings.Compare(a.server.IP, b.server.IP)
	})

	backends := make([]*backend.BackendServer, 0, len(entries))
	for i, e := range entries {
		e.server.ID = i + 1
		backends = append(backends, e.server)
	}

	return backends

}

// processServiceForConfig builds the backends of one bound service.
func processServiceForConfig(service corev1.Service, cfg config.Configuration, nodeIPs []string, warned *warnTracker) []*backend.BackendServer {
	var backends []*backend.BackendServer

	switch service.Spec.Type {
	case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		for _, port := range service.Spec.Ports {
			// NodePort is 0 when node port allocation is disabled
			// (allocateLoadBalancerNodePorts=false).
			if port.Name != cfg.BackendPortName || port.NodePort == 0 {
				continue
			}

			for _, nodeIP := range nodeIPs {
				backends = append(backends, backend.New(0, nodeIP, int(port.NodePort), port.Name))
			}
		}

	case corev1.ServiceTypeClusterIP:
		// A headless Service has no virtual IP to dial.
		if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
			break
		}

		for _, port := range service.Spec.Ports {
			// The ClusterIP listens on port.Port; TargetPort is the pod's.
			if port.Name != cfg.BackendPortName || port.Port <= 0 {
				continue
			}

			backends = append(backends, backend.New(0, service.Spec.ClusterIP, int(port.Port), port.Name))
		}

	default:
		if warned.shouldWarn("type/"+service.Namespace+"/"+service.Name, string(service.Spec.Type)) {
			emit.Warn.StructuredFields("Unsupported service type in centralized discovery, ignoring it",
				emit.ZString("service_type", string(service.Spec.Type)),
				emit.ZString("namespace", service.Namespace),
				emit.ZString("service_name", service.Name))
		}
	}

	return backends
}

// backendsEqual reports whether two backend slices hold the same multiset of
// address and port name, regardless of order.
func backendsEqual(old, new []*backend.BackendServer) bool {
	if len(old) != len(new) {
		return false
	}

	counts := make(map[string]int, len(old))
	for _, b := range old {
		counts[b.Address()+"/"+b.PortName]++
	}

	for _, b := range new {
		key := b.Address() + "/" + b.PortName
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}

	return true
}
