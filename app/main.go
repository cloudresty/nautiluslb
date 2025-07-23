package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/kubernetes"
	"github.com/cloudresty/nautiluslb/loadbalancer"
	"github.com/cloudresty/nautiluslb/utils"
)

func main() {

	// Configure emit logging
	// emit.SetComponent("nautiluslb")
	// emit.SetVersion(version.Version)
	emit.SetLevel("info")

	// Parse command line flags
	var showHelp = flag.Bool("help", false, "Show help information")
	flag.Parse()

	if *showHelp {
		fmt.Println("NautilusLB - Kubernetes-native Load Balancer")
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  nautiluslb [options]")
		fmt.Println()
		fmt.Println("Options:")
		fmt.Println("  -help        Show this help message")
		fmt.Println()
		fmt.Println("Configuration:")
		fmt.Println("  The application reads configuration from config.yaml in the current directory.")
		fmt.Println("  It automatically discovers Kubernetes services with the annotation:")
		fmt.Println("  nautiluslb.cloudresty.io/enabled=true")
		fmt.Println()
		fmt.Println("For more information, visit: https://github.com/cloudresty/nautiluslb")
		os.Exit(0)
	}

	emit.Info.Msg("Starting NautilusLB...")
	emit.Info.StructuredFields("Application Information",
		emit.ZString("app_name", "NautilusLB"),
		emit.ZString("repository", "https://github.com/cloudresty/nautiluslb"))
	emit.Info.Msg("Loading configuration...")

	//
	// Load configuration from YAML file
	//

	configData, err := utils.LoadConfig("config.yaml")
	if err != nil {
		emit.Error.StructuredFields("Failed to load configuration",
			emit.ZString("config_file", "config.yaml"),
			emit.ZString("error", err.Error()))
		os.Exit(1)
	}

	//
	// Initialize Kubernetes client
	//

	_, currentContext, err := kubernetes.GetK8sClient(configData.Settings.KubeconfigPath)
	if err != nil {
		emit.Error.StructuredFields("Failed to initialize Kubernetes client",
			emit.ZString("kubeconfig_path", configData.Settings.KubeconfigPath),
			emit.ZString("error", err.Error()))
		os.Exit(1)
	}
	emit.Info.StructuredFields("Initialized Kubernetes client",
		emit.ZString("context", currentContext))
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

		emit.Info.StructuredFields("Started load balancer",
			emit.ZString("config_name", backendConfig.Name),
			emit.ZString("listener_port", utils.ExtractPort(backendConfig.ListenerAddress)))

	}

	// Start centralized service discovery for all load balancers
	// Convert to interface slice
	var lbInterfaces []kubernetes.LoadBalancerInterface
	for _, lb := range loadBalancers {
		lbInterfaces = append(lbInterfaces, lb)
	}
	go kubernetes.DiscoverK8sServicesForAll(lbInterfaces, configData.BackendConfigurations)

	wg.Wait()
	emit.Info.Msg("All load balancers stopped, exiting")

	// Graceful shutdown on signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	emit.Info.Msg("Shutting down gracefully...")

	for _, backendConfig := range configData.BackendConfigurations {
		emit.Info.StructuredFields("Stopping load balancer",
			emit.ZString("config_name", backendConfig.Name))
		lb := loadbalancer.NewLoadBalancer(backendConfig, time.Duration(backendConfig.RequestTimeout)*time.Second)
		lb.Stop()
	}

	emit.Info.Msg("Shutdown complete.")
	os.Exit(0)

}
