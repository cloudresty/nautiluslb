package kubernetes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// LoadBalancerInterface defines the methods that DiscoverK8sServices needs from the LoadBalancer.
type LoadBalancerInterface interface {
	StartHealthChecks()
	GetMu() *sync.RWMutex
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

// defaultHealthCheckInterval is the interval in seconds between health checks.
var defaultHealthCheckInterval int = 30

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

// DiscoverK8sServices discovers services in Kubernetes and adds them as backends.
func DiscoverK8sServices(lb LoadBalancerInterface, config config.Configuration) {

	// Get the shared Kubernetes client, it should already be initialized
	k8sClient, err := GetSharedClient()

	if err != nil {
		return
	}

	backendCache := make(map[string]backend.BackendServer)

	watchServices := func() {

		for {

			sleepDuration := time.Duration(defaultHealthCheckInterval) * time.Second

			// The sleep duration is now always the default interval
			// since we removed config.HealthCheckInterval
			// If you want to make this configurable in the future, you'll need to

			// Use the namespace from config, default to all namespaces if empty
			namespace := config.Namespace
			if namespace == "" {
				namespace = "" // All namespaces (empty string means all namespaces)
			}

			services, err := k8sClient.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{})

			if err != nil {
				emit.Error.StructuredFields("Failed to list services",
					emit.ZString("namespace", namespace),
					emit.ZString("error", err.Error()))
				continue
			}

			lb.GetMu().Lock()

			// Create a map to track the new backends
			newBackends := make(map[string]*backend.BackendServer)
			nextBackendID := 1

			// Iterate over all services
			for _, service := range services.Items { // Check for the custom annotation
				if enabled, ok := service.Annotations["nautiluslb.cloudresty.io/enabled"]; ok && enabled == "true" {

					// Skip label selector check - just use annotation + namespace + port name
					// This allows services without specific labels to be discovered

					switch service.Spec.Type {
					case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:

						// For NodePort and LoadBalancer services, we can use the NodePort directly.
						for _, port := range service.Spec.Ports {

							nodeIPs := getNodeIPs()

							for _, nodeIP := range nodeIPs {
								backend := &backend.BackendServer{
									ID:       nextBackendID,
									IP:       nodeIP,
									Port:     int(port.NodePort),
									PortName: port.Name,
									Weight:   1,
									Healthy:  true,
								}
								newBackends[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = backend
								nextBackendID++

								// Use the service name from the Kubernetes API object
								serviceType := "NodePort" // or "LoadBalancer" depending on the actual type

								// Check if the backend is already in the cache
								if _, exists := backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)]; !exists {
									backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = *backend
								}

								// Update the cache with the new backend information
								existingBackend, ok := backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)]
								if ok && (existingBackend.IP != backend.IP || existingBackend.Port != backend.Port) {
									emit.Debug.StructuredFields("Updating backend",
										emit.ZString("service_name", service.Name),
										emit.ZString("service_type", serviceType),
										emit.ZString("backend_ip", backend.IP),
										emit.ZInt("backend_port", backend.Port))
									backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = *backend
								}

							}

							// Simplified: Use NodePort directly without pod discovery
							// This works with annotation-only approach

						}

					case corev1.ServiceTypeClusterIP:

						// For ClusterIP services, we use the ClusterIP and the target port.
						if len(service.Spec.Ports) > 0 {

							for _, port := range service.Spec.Ports {

								emit.Debug.StructuredFields("Found ClusterIP port",
									emit.ZString("port_name", port.Name),
									emit.ZInt("target_port", int(port.TargetPort.IntVal)))

								if port.TargetPort.IntVal > 0 {

									// Create a backend for each port of the ClusterIP service
									backend := &backend.BackendServer{
										ID:       nextBackendID,
										IP:       service.Spec.ClusterIP,
										Port:     int(port.TargetPort.IntVal),
										PortName: port.Name,
										Weight:   1,
										Healthy:  true,
									}

									newBackends[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = backend
									nextBackendID++

								} else {

									emit.Warn.StructuredFields("Skipping port - TargetPort not defined",
										emit.ZString("port_name", port.Name))

								}

							}

						} else {

							emit.Warn.StructuredFields("No ports found for ClusterIP service",
								emit.ZString("service_name", service.Name))

						}

					default:
						emit.Warn.StructuredFields("Service type not supported",
							emit.ZString("service_type", string(service.Spec.Type)),
							emit.ZString("service_name", service.Name))

					}

				}

			}

			// Compare new backends with existing backends
			existingBackends := lb.GetBackendServers()
			backendsChanged := false

			if len(newBackends) != len(existingBackends) {

				backendsChanged = true

			} else {

				for _, newBackend := range newBackends {

					found := false

					for _, existingBackend := range existingBackends {

						if newBackend.IP == existingBackend.IP && newBackend.Port == existingBackend.Port {
							found = true
							break
						}

					}

					if !found {
						backendsChanged = true
						break
					}

				}

			}

			if backendsChanged {

				// Clear existing backends before adding new ones from K8s
				lb.SetBackendServers([]*backend.BackendServer{})

				// Accumulate the new backends in a temporary list
				var backendList []*backend.BackendServer

				// Add the new backends to the list
				for _, backend := range newBackends {
					backendList = append(backendList, backend)
				}

				lb.SetBackendServers(backendList)

			}

			lb.GetMu().Unlock()

			time.Sleep(sleepDuration) // Sleep before re-listing

			if backendsChanged {

				emit.Info.Msg("Backend servers changed, updating background health checks")
				lb.StartHealthChecks()
				emit.Info.Msg("Background health checks configuration updated")

			} else {
				// Backend servers unchanged, skipping background health checks configuration update
				emit.Debug.Msg("Backend servers unchanged")
			}
		}

	}

	go watchServices()

}

func getNodeIPs() []string {

	nodes, err := sharedK8sClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		emit.Error.StructuredFields("Failed to list nodes",
			emit.ZString("error", err.Error()))
		return []string{}
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

	return ips

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

	// Main discovery loop
	for {
		sleepDuration := time.Duration(defaultHealthCheckInterval) * time.Second

		// Group configs by namespace for efficient API calls
		namespaceConfigs := make(map[string][]config.Configuration)
		for _, cfg := range configs {
			namespace := cfg.Namespace
			if namespace == "" {
				namespace = "all" // Special key for all namespaces
			}
			namespaceConfigs[namespace] = append(namespaceConfigs[namespace], cfg)
		}

		// Discover services per namespace
		for namespace, nsConfigs := range namespaceConfigs {
			discoverServicesForNamespace(k8sClient, namespace, nsConfigs, configToLB)
		}

		time.Sleep(sleepDuration)
	}
}

// discoverServicesForNamespace discovers services in a specific namespace for centralized discovery
func discoverServicesForNamespace(k8sClient *Clientset, namespace string, configs []config.Configuration, configToLB map[string]LoadBalancerInterface) {
	// Use empty string for all namespaces
	searchNamespace := namespace
	if namespace == "all" {
		searchNamespace = ""
	}

	services, err := k8sClient.CoreV1().Services(searchNamespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		emit.Error.StructuredFields("Failed to list services in centralized discovery",
			emit.ZString("namespace", namespace),
			emit.ZString("error", err.Error()))
		return
	}

	// Process each configuration
	for _, cfg := range configs {
		backends := processServicesForConfig(services.Items, cfg)

		// Update the corresponding LoadBalancer
		if lb, exists := configToLB[cfg.Name]; exists {
			currentBackends := lb.GetBackendServers()

			// Only update if backends changed
			if !backendsEqual(currentBackends, backends) {
				lb.SetBackendServers(backends)
				emit.Info.StructuredFields("Updated backends for config",
					emit.ZInt("backend_count", len(backends)),
					emit.ZString("config_name", cfg.Name))

				// Start health checks
				go lb.StartHealthChecks()
			}
		}
	}
}

// processServicesForConfig processes services for a specific configuration in centralized discovery
func processServicesForConfig(services []corev1.Service, cfg config.Configuration) []*backend.BackendServer {
	var backends []*backend.BackendServer
	backendID := 1

	for _, service := range services {
		// Check for annotation
		if enabled, ok := service.Annotations["nautiluslb.cloudresty.io/enabled"]; !ok || enabled != "true" {
			continue
		}

		// Skip label selector check - just use annotation + namespace + port name
		// This allows services without specific labels to be discovered

		// Process the service based on type
		serviceBackends := processServiceForConfig(service, cfg, &backendID)
		backends = append(backends, serviceBackends...)
	}

	return backends
}

// processServiceForConfig processes a single service for centralized discovery
func processServiceForConfig(service corev1.Service, cfg config.Configuration, backendID *int) []*backend.BackendServer {
	var backends []*backend.BackendServer

	switch service.Spec.Type {
	case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		for _, port := range service.Spec.Ports {
			if port.Name != cfg.BackendPortName {
				continue
			}

			nodeIPs := getNodeIPs()
			for _, nodeIP := range nodeIPs {
				backend := &backend.BackendServer{
					ID:       *backendID,
					IP:       nodeIP,
					Port:     int(port.NodePort),
					PortName: port.Name,
					Weight:   1,
					Healthy:  true,
				}
				backends = append(backends, backend)
				*backendID++
			}
		}

	case corev1.ServiceTypeClusterIP:
		for _, port := range service.Spec.Ports {
			if port.Name != cfg.BackendPortName {
				continue
			}

			if port.TargetPort.IntVal > 0 {
				backend := &backend.BackendServer{
					ID:       *backendID,
					IP:       service.Spec.ClusterIP,
					Port:     int(port.TargetPort.IntVal),
					PortName: port.Name,
					Weight:   1,
					Healthy:  true,
				}
				backends = append(backends, backend)
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
		oldMap[fmt.Sprintf("%s:%d", b.IP, b.Port)] = b
	}

	for _, b := range new {
		key := fmt.Sprintf("%s:%d", b.IP, b.Port)
		if _, exists := oldMap[key]; !exists {
			return false
		}
	}

	return true
}
