package kubernetes

import (
	"context"
	"fmt"
	"log"
	"net"
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
	k8sClient *kubernetes.Clientset
)

// LoadBalancerInterface defines the methods that DiscoverK8sServices needs from the LoadBalancer.
type LoadBalancerInterface interface {
	StartHealthChecks()
	GetMu() *sync.RWMutex
	GetBackendServers() []*backend.BackendServer
	SetBackendServers(servers []*backend.BackendServer)
	GetListener() net.Listener
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

	return clientset, currentContext, nil

}

// DiscoverK8sServices discovers services in Kubernetes and adds them as backends.
func DiscoverK8sServices(lb LoadBalancerInterface, config config.BackendConfiguration) {

	// Initialize Kubernetes client if not already initialized
	var (
		initClient     sync.Once
		currentContext string
		err            error
	)

	initClient.Do(func() {
		k8sClient, currentContext, err = GetK8sClient("")
	})

	if err != nil {
		log.Printf("Failed to initialize Kubernetes client: %v", err)
		os.Exit(1)
	}

	log.Printf("Initialized Kubernetes client using local context: %s", currentContext)

	if k8sClient == nil {
		return
	}

	for {

		sleepDuration := 10 * time.Second

		if config.HealthCheckInterval > 0 {
			sleepDuration = time.Duration(config.HealthCheckInterval) * time.Second

			if config.HealthCheckInterval < 10 {

				sleepDuration = 10 * time.Second
				log.Printf("Health check interval is too low, setting it to 10 seconds")

			} else {

				sleepDuration = time.Duration(config.HealthCheckInterval) * time.Second
			}

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
					log.Printf("Found annotated service: %s/%s", service.Namespace, service.Name)

					if service.Spec.Type == corev1.ServiceTypeNodePort || service.Spec.Type == corev1.ServiceTypeLoadBalancer {

						for _, port := range service.Spec.Ports {

							log.Printf("Found port: %s", port.Name)

							// Find the nodes that are running the pods
							pods, err := k8sClient.CoreV1().Pods(service.Namespace).List(context.TODO(), metav1.ListOptions{
								LabelSelector: config.LabelSelector,
							})

							if err != nil {
								log.Printf("Failed to list pods for service %s: %v", service.Name, err)
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

									log.Printf("Adding backend: %s:%d", backend.IP, backend.Port)

								}

							}

						}

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
			lb.StartHealthChecks()

			log.Printf("Discovered %d services from Kubernetes", len(services.Items))

			if len(annotatedServices) > 0 {
				log.Printf("Annotated services: %v", annotatedServices)
			}

			time.Sleep(sleepDuration)

		}

	}

}
