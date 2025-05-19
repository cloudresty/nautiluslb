package kubernetes

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

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

		log.Println("Using in-cluster Kubernetes config")
		currentContext = "in-cluster"

	} else {

		log.Println("Failed to get in-cluster config:", err)

		// Fallback to kubeconfig file
		if kubeconfigPath == "" {

			log.Println("KUBECONFIG environment variable not set, using default ~/.kube/config")

			home, err := os.UserHomeDir()
			if err != nil {
				return nil, "", fmt.Errorf("failed to get user home directory: %v", err)
			}

			kubeconfigPath = filepath.Join(home, ".kube", "config")

		} else {

			log.Printf("Using KUBECONFIG: %s", kubeconfigPath)

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

// DiscoverK8sServices discovers services in Kubernetes and adds them as backends.
func DiscoverK8sServices(lb LoadBalancerInterface, config config.BackendConfiguration) {

	// Get the shared Kubernetes client, it should already be initialized
	k8sClient, err := GetSharedClient()

	if err != nil {
		return
	}

	backendCache := make(map[string]backend.BackendServer)

	watchServices := func() {

		for {

			sleepDuration := 60 * time.Second

			if config.HealthCheckInterval > 0 {

				sleepDuration = time.Duration(config.HealthCheckInterval) * time.Second

				if config.HealthCheckInterval < 60 {

					sleepDuration = 60 * time.Second
					log.Printf("Health check interval is too low, setting it to 10 seconds")

				}

			}

			sleepDuration = time.Duration(config.HealthCheckInterval) * time.Second

			services, err := k8sClient.CoreV1().Services("").List(context.TODO(), metav1.ListOptions{})

			if err != nil {
				log.Printf("Failed to list services: %v", err)
				continue
			}

			lb.GetMu().Lock()

			// Create a map to track the new backends
			newBackends := make(map[string]*backend.BackendServer)
			nextBackendID := 1
			var annotatedServices []string

			for _, service := range services.Items {

				// Check for the custom annotation
				if enabled, ok := service.Annotations["nautiluslb.cloudresty.io/enabled"]; ok && enabled == "true" {

					annotatedServices = append(annotatedServices, fmt.Sprintf("%s/%s", service.Namespace, service.Name))
					log.Printf("Discovered annotated service '%s/%s'", service.Namespace, service.Name)

					switch service.Spec.Type {
					case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:

						// For NodePort and LoadBalancer services, we can use the NodePort directly.
						for _, port := range service.Spec.Ports {

							log.Printf("Found 'spec.ports.name: %s' - 'spec.ports.nodePort: %d'", port.Name, port.NodePort)
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
									log.Printf("Adding backend for '%s' with type '%s' towards '%s:%d'", service.Name, serviceType, backend.IP, backend.Port)
									backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = *backend
								}

								// Update the cache with the new backend information
								existingBackend, ok := backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)]
								if ok && (existingBackend.IP != backend.IP || existingBackend.Port != backend.Port) {
									log.Printf("Updating backend for '%s' with type '%s' towards '%s:%d'", service.Name, serviceType, backend.IP, backend.Port)
									backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = *backend
								}

							}

							if config.LabelSelector != "" {
								pods, err := k8sClient.CoreV1().Pods(service.Namespace).List(context.TODO(), metav1.ListOptions{
									LabelSelector: config.LabelSelector,
								})
								if err != nil {
									log.Printf("Failed to list pods for service '%s': %v", service.Name, err)
									continue
								}

								for _, pod := range pods.Items {

									if pod.Status.Phase == corev1.PodRunning {
										backend := &backend.BackendServer{
											ID:       nextBackendID,
											IP:       pod.Status.HostIP,
											Port:     int(port.NodePort),
											PortName: port.Name,
											Weight:   1,
											Healthy:  true,
										}

										newBackends[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = backend
										nextBackendID++
										log.Printf("Adding backend (NodePort/LoadBalancer): %s:%d", backend.IP, backend.Port)

									}

								}

							} else {

								log.Printf("Label selector is empty. Cannot determine backend pods for NodePort/LoadBalancer service '%s'", service.Name)

							}

						}

					case corev1.ServiceTypeClusterIP:

						// For ClusterIP services, we use the ClusterIP and the target port.
						if len(service.Spec.Ports) > 0 {

							for _, port := range service.Spec.Ports {

								log.Printf("Found ClusterIP port: %s - TargetPort: %d", port.Name, port.TargetPort.IntVal)

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
									log.Printf("Adding backend (ClusterIP): %s:%d", backend.IP, backend.Port)

								} else {

									log.Printf("Skipping port '%s' because TargetPort is not defined or invalid.", port.Name)

								}

							}

						} else {

							log.Printf("No ports found for ClusterIP service '%s'", service.Name)

						}

					default:
						log.Printf("Service type '%s' not supported for service '%s'", service.Spec.Type, service.Name)

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

				for _, backend := range newBackends {
					backendList = append(backendList, backend)
				}

				lb.SetBackendServers(backendList)

			}

			lb.GetMu().Unlock()

			if len(annotatedServices) > 0 {
				log.Printf("NautilusLB annotated services '%v' out of '%d' discovered Kubernetes services: %v", len(annotatedServices), len(services.Items), annotatedServices)
			}

			time.Sleep(sleepDuration) // Sleep before re-listing

			if backendsChanged {

				lb.StartHealthChecks()
				log.Printf("Health checks started")

			}

		}

	}

	go watchServices()

}

func getNodeIPs() []string {

	nodes, err := sharedK8sClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list nodes: %v", err)
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
