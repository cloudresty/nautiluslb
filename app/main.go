package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/config"
	"github.com/cloudresty/nautiluslb/kubernetes"
	"github.com/cloudresty/nautiluslb/loadbalancer"
	"github.com/cloudresty/nautiluslb/utils"
)

func main() {

	// Configure emit logging
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
		fmt.Println("  Each configuration must list the namespaces it discovers Services in")
		fmt.Println("  (namespaces: [a, b]), or namespaces: [\"*\"] for cluster-wide discovery.")
		fmt.Println()
		fmt.Println("  A Service is a backend of a configuration only when all of these hold:")
		fmt.Println("    " + config.ServiceEnabledAnnotation + ": \"true\"")
		fmt.Println("    " + config.ServiceConfigurationsAnnotation + ": \"<name>[,<name>...]\" names the configuration")
		fmt.Println("    the Service is in one of the configuration's namespaces")
		fmt.Println("    a Service port is named the configuration's backendPortName")
		fmt.Println()
		fmt.Println("  See config.example.yaml for a complete example.")
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
	// Install the signal handler before anything starts, so SIGTERM (docker
	// stop) always reaches the shutdown path below.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	var wg sync.WaitGroup
	var loadBalancers []*loadbalancer.LoadBalancer

	//
	// Create a new load balancer for each backend configuration
	//

	for _, backendConfig := range configData.BackendConfigurations {

		// Parse the duration string into a time.Duration
		duration := time.Duration(backendConfig.RequestTimeout) * time.Second

		loadBalancers = append(loadBalancers, loadbalancer.NewLoadBalancer(backendConfig, duration))

	}

	// Bind every listener before serving any, so a port in use is a clear
	// startup error rather than a crash in a goroutine.
	if addr, err := bindAll(loadBalancers); err != nil {
		emit.Error.StructuredFields("Failed to bind listener",
			emit.ZString("listener_addr", addr),
			emit.ZString("error", err.Error()))
		os.Exit(1)
	}

	for i, lb := range loadBalancers {

		wg.Add(1)
		go func(lb *loadbalancer.LoadBalancer) {
			defer wg.Done()
			lb.Start()
		}(lb)

		emit.Info.StructuredFields("Started load balancer",
			emit.ZString("config_name", configData.BackendConfigurations[i].Name),
			emit.ZString("listener_port", utils.ExtractPort(lb.ListenerAddress)))

	}

	// Start centralized service discovery for all load balancers
	discoveryCtx, stopDiscovery := context.WithCancel(context.Background())
	var lbInterfaces []kubernetes.LoadBalancerInterface
	for _, lb := range loadBalancers {
		lbInterfaces = append(lbInterfaces, lb)
	}
	discoveryDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(discoveryDone)
		kubernetes.DiscoverK8sServicesForAll(discoveryCtx, lbInterfaces, configData.BackendConfigurations)
	}()

	sig := <-sigChan
	emit.Info.StructuredFields("Shutting down gracefully...",
		emit.ZString("signal", sig.String()))

	// Stop discovery and wait for it to exit, so it cannot update a load
	// balancer being stopped. The wait is bounded: in-flight API calls return
	// promptly once the context is cancelled, but shutdown must never hang.
	stopDiscovery()
	select {
	case <-discoveryDone:
	case <-time.After(5 * time.Second):
		emit.Warn.Msg("Service discovery did not stop within 5s, continuing shutdown")
	}

	// Stop the load balancers that are actually running. Connections being
	// proxied end when the process exits.
	for _, lb := range loadBalancers {
		emit.Info.StructuredFields("Stopping load balancer",
			emit.ZString("listener_addr", lb.ListenerAddress))
		lb.Stop()
	}

	wg.Wait()

	emit.Info.Msg("Shutdown complete.")
	os.Exit(0)

}

// bindAll binds every load balancer's listener. If any bind fails, the ones
// already bound are stopped, and the failing listener address is returned.
func bindAll(loadBalancers []*loadbalancer.LoadBalancer) (string, error) {

	for i, lb := range loadBalancers {
		if err := lb.Listen(); err != nil {
			for _, bound := range loadBalancers[:i] {
				bound.Stop()
			}
			return lb.ListenerAddress, fmt.Errorf("binding listener: %w", err)
		}
	}

	return "", nil

}
