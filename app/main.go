package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/cloudresty/nautiluslb/kubernetes"

	"github.com/cloudresty/nautiluslb/loadbalancer"
	"github.com/cloudresty/nautiluslb/utils"
)

func main() {

	version := "v0.0.6"

	asciiArt := `
 _   _             _   _ _           _     ____
| \ | | __ _ _   _| |_(_) |_   _ ___| |   | __ )
|  \| |/ _' | | | | __| | | | | / __| |   |  _ \
| |\  | (_| | |_| | |_| | | |_| \__ \ |___| |_) |
|_| \_|\__,_|\__,_|\__|_|_|\__,_|___/_____|____/
`
	fmt.Println(asciiArt)
	fmt.Println("NautilusLB" + " " + version + " " + "(alpha)")
	fmt.Println("https://github.com/cloudresty/nautiluslb")
	fmt.Println()
	fmt.Println("---")
	fmt.Println()

	//
	// Load configuration from YAML file
	//

	configData, err := utils.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("System | Failed to load configuration: %v", err)
		os.Exit(1)
	}

	//
	// Initialize Kubernetes client
	//

	_, currentContext, err := kubernetes.GetK8sClient(configData.Settings.KubeconfigPath)
	if err != nil {
		log.Fatalf("System | Failed to initialize Kubernetes client: %v", err)
		os.Exit(1)
	}
	log.Printf("System | Initialized Kubernetes client using context: %s", currentContext)
	var wg sync.WaitGroup

	//
	// Create a new load balancer for each backend configuration
	//

	for _, backendConfig := range configData.BackendConfigurations {

		wg.Add(1)

		// Parse the duration string into a time.Duration
		duration := time.Duration(backendConfig.RequestTimeout) * time.Second

		lb := loadbalancer.NewLoadBalancer(backendConfig, duration)

		// Start Kubernetes service discovery, passing the client
		go lb.DiscoverK8sServices()

		// Start the load balancer
		go func(lb *loadbalancer.LoadBalancer) {
			defer wg.Done()
			lb.Start()
		}(lb)

		log.Printf("System | Started load balancer: %s > %s", backendConfig.Name, utils.ExtractPort(backendConfig.ListenerAddress))

	}

	wg.Wait()
	log.Println("System | All load balancers stopped, exiting")

	// Graceful shutdown on signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, os.Kill)
	<-sigChan

	log.Println("System | Shutting down gracefully...")

	for _, backendConfig := range configData.BackendConfigurations {
		log.Printf("System | Stopping load balancer for '%s'", backendConfig.Name)
		lb := loadbalancer.NewLoadBalancer(backendConfig, time.Duration(backendConfig.RequestTimeout)*time.Second)
		lb.Stop()
	}

	log.Println("System | Shutdown complete.")
	os.Exit(0)

}
