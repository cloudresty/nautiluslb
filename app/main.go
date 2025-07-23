package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cloudresty/nautiluslb/kubernetes"
	"github.com/cloudresty/nautiluslb/loadbalancer"
	"github.com/cloudresty/nautiluslb/utils"
	"github.com/cloudresty/nautiluslb/version"
)

func main() {

	// Parse command line flags
	var showHelp = flag.Bool("help", false, "Show help information")
	var showVersion = flag.Bool("version", false, "Show version information")
	flag.Parse()

	if *showHelp {
		fmt.Println("NautilusLB - Kubernetes-native Load Balancer")
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  nautiluslb [options]")
		fmt.Println()
		fmt.Println("Options:")
		fmt.Println("  -help        Show this help message")
		fmt.Println("  -version     Show version information")
		fmt.Println()
		fmt.Println("Configuration:")
		fmt.Println("  The application reads configuration from config.yaml in the current directory.")
		fmt.Println("  It automatically discovers Kubernetes services with the annotation:")
		fmt.Println("  nautiluslb.cloudresty.io/enabled=true")
		fmt.Println()
		fmt.Println("For more information, visit: https://github.com/cloudresty/nautiluslb")
		os.Exit(0)
	}

	if *showVersion {
		buildInfo := version.Get()
		fmt.Println("NautilusLB")
		fmt.Println(buildInfo.DetailedString())
		os.Exit(0)
	}

	asciiArt := `
 _   _             _   _ _           _     ____
| \ | | __ _ _   _| |_(_) |_   _ ___| |   | __ )
|  \| |/ _' | | | | __| | | | | / __| |   |  _ \
| |\  | (_| | |_| | |_| | | |_| \__ \ |___| |_) |
|_| \_|\__,_|\__,_|\__|_|_|\__,_|___/_____|____/
`
	fmt.Println(asciiArt)
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
	var loadBalancers []*loadbalancer.LoadBalancer

	//
	// Create a new load balancer for each backend configuration (without individual discovery)
	//

	for _, backendConfig := range configData.BackendConfigurations {

		wg.Add(1)

		// Parse the duration string into a time.Duration
		duration := time.Duration(backendConfig.RequestTimeout) * time.Second

		lb := loadbalancer.NewLoadBalancer(backendConfig, duration)
		loadBalancers = append(loadBalancers, lb)

		// Start the load balancer
		go func(lb *loadbalancer.LoadBalancer) {
			defer wg.Done()
			lb.Start()
		}(lb)

		log.Printf("System | Started load balancer: %s > %s", backendConfig.Name, utils.ExtractPort(backendConfig.ListenerAddress))

	}

	// Start centralized service discovery for all load balancers
	// Convert to interface slice
	var lbInterfaces []kubernetes.LoadBalancerInterface
	for _, lb := range loadBalancers {
		lbInterfaces = append(lbInterfaces, lb)
	}
	go kubernetes.DiscoverK8sServicesForAll(lbInterfaces, configData.BackendConfigurations)

	wg.Wait()
	log.Println("System | All load balancers stopped, exiting")

	// Graceful shutdown on signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
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
