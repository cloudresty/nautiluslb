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

		log.Println("System | Using in-cluster Kubernetes config")
		currentContext = "in-cluster"

	} else {

		log.Println("System | Failed to get in-cluster config:", err)

		// Fallback to kubeconfig file
		if kubeconfigPath == "" {

			log.Println("System | KUBECONFIG environment variable not set, using default ~/.kube/config")

			home, err := os.UserHomeDir()
			if err != nil {
				return nil, "", fmt.Errorf("failed to get user home directory: %v", err)
			}

			kubeconfigPath = filepath.Join(home, ".kube", "config")

		} else {

			log.Printf("System | Using KUBECONFIG: %s", kubeconfigPath)

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
			services, err := k8sClient.CoreV1().Services("").List(context.TODO(), metav1.ListOptions{})

			if err != nil {
				log.Printf("System | Failed to list services: %v", err)
				continue
			}

			lb.GetMu().Lock()

			// Create a map to track the new backends
			newBackends := make(map[string]*backend.BackendServer)
			nextBackendID := 1
			var annotatedServices []string

			// Iterate over all services
			for _, service := range services.Items {

				// Check for the custom annotation
				if enabled, ok := service.Annotations["nautiluslb.cloudresty.io/enabled"]; ok && enabled == "true" {

					annotatedServices = append(annotatedServices, fmt.Sprintf("%s/%s", service.Namespace, service.Name))
					// log.Printf("Discovered annotated service '%s/%s'", service.Namespace, service.Name)

					switch service.Spec.Type {
					case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:

						// For NodePort and LoadBalancer services, we can use the NodePort directly.
						for _, port := range service.Spec.Ports {

							// log.Printf("Discovered annotated service '%s/%s', type '%s', port name '%s' and port number '%d'", service.Namespace, service.Name, service.Spec.Type, port.Name, port.NodePort)

							// log.Printf("Found 'spec.ports.name: %s' - 'spec.ports.nodePort: %d'", port.Name, port.NodePort)
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
									// log.Printf("System | Adding backend (%d): %s > %s > %s:%d", i+1, service.Name, serviceType, backend.IP, backend.Port)
									backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = *backend
								}

								// Update the cache with the new backend information
								existingBackend, ok := backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)]
								if ok && (existingBackend.IP != backend.IP || existingBackend.Port != backend.Port) {
									log.Printf("System | Updating backend: %s >%s > %s:%d'", service.Name, serviceType, backend.IP, backend.Port)
									backendCache[fmt.Sprintf("%s:%d", backend.IP, backend.Port)] = *backend
								}

							}

							if config.BackendLabelSelector != "" {
								pods, err := k8sClient.CoreV1().Pods(service.Namespace).List(context.TODO(), metav1.ListOptions{
									LabelSelector: config.BackendLabelSelector,
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
										// log.Printf("Adding backend (NodePort/LoadBalancer): %s:%d", backend.IP, backend.Port)

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
									// log.Printf("Adding backend (ClusterIP): %s:%d", backend.IP, backend.Port)

								} else {

									log.Printf("Skipping port '%s' because TargetPort is not defined or invalid.", port.Name)

								}

							}

						} else {

							log.Printf("No ports found for ClusterIP service '%s'", service.Name)

						}

					default:
						log.Printf("System | Service type '%s' not supported for service '%s'", service.Spec.Type, service.Name)

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

			// if len(annotatedServices) > 0 {
			// log.Printf("System | K8s annotated services (%v/%d): %v", len(annotatedServices), len(services.Items), annotatedServices)
			// }

			time.Sleep(sleepDuration) // Sleep before re-listing

			if backendsChanged {

				log.Println("System | Backend servers changed, updating background health checks")
				lb.StartHealthChecks()
				log.Println("System | Background health checks configuration updated")

			} else {

				// log.Println("System | Backend servers unchanged, skipping background health checks configuration update")

			}

		}

	}

	go watchServices()

}

func getNodeIPs() []string {

	nodes, err := sharedK8sClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Printf("System | Failed to list nodes: %v", err)
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
