package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/cloudresty/nautiluslb/kubernetes"

	"github.com/cloudresty/nautiluslb/config"
	"github.com/cloudresty/nautiluslb/loadbalancer"
	"github.com/cloudresty/nautiluslb/utils"
	"gopkg.in/yaml.v3"
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

	// Load configuration from YAML file
	configData, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
		os.Exit(1)
	}

	// Initialize Kubernetes client
	_, currentContext, err := kubernetes.GetK8sClient(configData.Settings.KubeconfigPath)
	if err != nil {
		log.Fatalf("Failed to initialize Kubernetes client: %v", err)
		os.Exit(1)
	}
	log.Printf("Initialized Kubernetes client using local context '%s'", currentContext)
	var wg sync.WaitGroup

	// Create a new load balancer for each backend configuration
	for _, backendConfig := range configData.BackendConfigurations {

		wg.Add(1)

		// Parse the duration string into a time.Duration
		duration := time.Duration(backendConfig.RequestTimeout) * time.Second

		lb := loadbalancer.NewLoadBalancer(backendConfig, duration)

		// Start Kubernetes service discovery, passing the client
		go lb.DiscoverK8sServices()

		log.Printf("Started load balancer for '%s' on port '%s'", backendConfig.Name, utils.ExtractPort(backendConfig.ListenerAddress))

		// Start the load balancer
		go func(lb *loadbalancer.LoadBalancer) {
			defer wg.Done()
			lb.Start()
		}(lb)

	}

	wg.Wait()
	log.Println("All load balancers stopped, exiting")

	// Graceful shutdown on signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, os.Kill)
	<-sigChan

	log.Println("Shutting down gracefully...")
	// Add any cleanup or finalization logic here, e.g., closing database connections

	log.Println("Shutdown complete.")
	os.Exit(0)

}

func loadConfig(filename string) (config.Config, error) {

	// Read the YAML file (config.yaml)
	data, err := os.ReadFile(filename)
	if err != nil {
		return config.Config{}, err
	}

	// Unmarshal the YAML data into the Config struct
	var configData config.Config
	err = yaml.Unmarshal(data, &configData)
	if err != nil {
		return config.Config{}, err
	}

	// Validate backend configurations
	for i, bc := range configData.BackendConfigurations {

		if err := bc.Validate(); err != nil {
			return config.Config{}, fmt.Errorf("invalid backend configuration at index %d: %v", i, err)
		}

	}

	return configData, nil

}
