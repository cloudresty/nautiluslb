package kubernetes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/backend"
	"github.com/cloudresty/nautiluslb/config"
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

// matchesLabelSelector checks if service labels match the given label selector
func matchesLabelSelector(serviceLabels map[string]string, labelSelector string) bool {
	if labelSelector == "" {
		return true // Empty selector matches everything
	}

	// Parse label selector (format: "key1=value1,key2=value2")
	pairs := strings.Split(labelSelector, ",")
	for _, pair := range pairs {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if serviceLabels[key] != value {
			return false
		}
	}
	return true
}

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

// DiscoverK8sServicesForAll discovers services for all load balancers centrally
func DiscoverK8sServicesForAll(loadBalancers []LoadBalancerInterface, configs []config.Configuration) {

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
	for i, config := range configs {
		if i < len(loadBalancers) {
			configToLB[config.Name] = loadBalancers[i]
		}
	}

	// Group configs by namespace for efficient API calls
	namespaceConfigs := make(map[string][]config.Configuration)
	for _, cfg := range configs {
		namespace := cfg.Namespace
		if namespace == "" {
			namespace = "all" // Special key for all namespaces
		}
		namespaceConfigs[namespace] = append(namespaceConfigs[namespace], cfg)
	}

	// Main discovery loop
	for {

		discoverOnce(context.Background(), k8sClient, namespaceConfigs, configToLB)

		time.Sleep(discoveryInterval)
	}
}

// discoverOnce runs one discovery pass. Node IPs are listed once per pass.
// If that fails the pass is skipped: an empty node list would otherwise
// replace every NodePort backend with nothing, turning an API blip (or an
// expired credential) into an outage.
func discoverOnce(ctx context.Context, k8sClient kubernetes.Interface, namespaceConfigs map[string][]config.Configuration, configToLB map[string]LoadBalancerInterface) {

	nodeIPs, err := getNodeIPs(ctx, k8sClient)
	if err != nil {
		emit.Error.StructuredFields("Failed to list nodes, keeping current backends",
			emit.ZString("error", err.Error()))
		return
	}

	for namespace, nsConfigs := range namespaceConfigs {
		discoverServicesForNamespace(ctx, k8sClient, namespace, nsConfigs, configToLB, nodeIPs)
	}

}

// discoverServicesForNamespace discovers services in a specific namespace for centralized discovery
func discoverServicesForNamespace(ctx context.Context, k8sClient kubernetes.Interface, namespace string, configs []config.Configuration, configToLB map[string]LoadBalancerInterface, nodeIPs []string) {
	// Use empty string for all namespaces
	searchNamespace := namespace
	if namespace == "all" {
		searchNamespace = ""
	}

	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	services, err := k8sClient.CoreV1().Services(searchNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		emit.Error.StructuredFields("Failed to list services in centralized discovery, keeping current backends",
			emit.ZString("namespace", namespace),
			emit.ZString("error", err.Error()))
		return
	}

	// Process each configuration
	for _, cfg := range configs {
		backends := processServicesForConfig(services.Items, cfg, nodeIPs)

		// Update the corresponding LoadBalancer
		if lb, exists := configToLB[cfg.Name]; exists {
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
}

// processServicesForConfig processes services for a specific configuration in centralized discovery
func processServicesForConfig(services []corev1.Service, cfg config.Configuration, nodeIPs []string) []*backend.BackendServer {
	var backends []*backend.BackendServer
	backendID := 1

	for _, service := range services {
		// Check for annotation
		if enabled, ok := service.Annotations["nautiluslb.cloudresty.io/enabled"]; !ok || enabled != "true" {
			continue
		}

		// Process the service based on type
		serviceBackends := processServiceForConfig(service, cfg, nodeIPs, &backendID)
		backends = append(backends, serviceBackends...)
	}

	return backends
}

// processServiceForConfig processes a single service for centralized discovery
func processServiceForConfig(service corev1.Service, cfg config.Configuration, nodeIPs []string, backendID *int) []*backend.BackendServer {
	var backends []*backend.BackendServer

	switch service.Spec.Type {
	case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		for _, port := range service.Spec.Ports {
			if port.Name != cfg.BackendPortName {
				continue
			}

			for _, nodeIP := range nodeIPs {
				backends = append(backends, backend.New(*backendID, nodeIP, int(port.NodePort), port.Name))
				*backendID++
			}
		}

	case corev1.ServiceTypeClusterIP:
		for _, port := range service.Spec.Ports {
			if port.Name != cfg.BackendPortName {
				continue
			}

			if port.TargetPort.IntVal > 0 {
				backends = append(backends, backend.New(*backendID, service.Spec.ClusterIP, int(port.TargetPort.IntVal), port.Name))
				*backendID++
			}
		}

	default:
		emit.Warn.StructuredFields("Unsupported service type in centralized discovery",
			emit.ZString("service_type", string(service.Spec.Type)),
			emit.ZString("service_name", service.Name))
	}

	return backends
}

// backendsEqual compares two backend slices for centralized discovery
func backendsEqual(old, new []*backend.BackendServer) bool {
	if len(old) != len(new) {
		return false
	}

	// Create maps for comparison
	oldMap := make(map[string]*backend.BackendServer)
	for _, b := range old {
		oldMap[b.Address()+"/"+b.PortName] = b
	}

	for _, b := range new {
		if _, exists := oldMap[b.Address()+"/"+b.PortName]; !exists {
			return false
		}
	}

	return true
}
