package loadbalancer

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/cloudresty/nautiluslb/backend"
	"github.com/cloudresty/nautiluslb/config"
	"github.com/cloudresty/nautiluslb/kubernetes"
	"github.com/cloudresty/nautiluslb/utils"
)

// LoadBalancer represents the load balancer.
type LoadBalancer struct {
	backendServers   []*backend.BackendServer
	nextServer       int
	Listener         net.Listener
	listenerAddr     string
	mu               sync.RWMutex
	stopChan         chan struct{}
	stopHealthChecks chan struct{}
	healthCheckMap   map[string]bool
	healthCheckCache map[string]bool                // Cache for health check status
	portBackendMap   map[int]*backend.BackendServer // Listener port to backend server mapping
	config           config.BackendConfiguration
	requestTimeout   time.Duration
	ListenerAddress  string
}

// NewLoadBalancer creates a new LoadBalancer instance.
func NewLoadBalancer(config config.BackendConfiguration, requestTimeout time.Duration) *LoadBalancer {

	lb := &LoadBalancer{
		backendServers:   []*backend.BackendServer{},
		listenerAddr:     config.ListenerAddress,
		healthCheckMap:   make(map[string]bool),
		config:           config,
		requestTimeout:   requestTimeout,
		stopChan:         make(chan struct{}),
		stopHealthChecks: make(chan struct{}),
		ListenerAddress:  config.ListenerAddress,
		healthCheckCache: make(map[string]bool),
	}
	lb.Listener = nil

	go lb.StartHealthChecks()
	return lb
}

// Start starts the load balancer.
func (lb *LoadBalancer) Start() {

	log.Printf("Starting health checks...")
	go lb.StartHealthChecks()
	log.Printf("Health checks started...")

	var err error
	lb.Listener, err = net.Listen("tcp", lb.listenerAddr)
	if err != nil {
		log.Fatalf("Failed to listen on port '%s': %v", utils.ExtractPort(lb.listenerAddr), err)
	}

	log.Printf("Listening on port '%s'", utils.ExtractPort(lb.listenerAddr))

	listener := lb.GetListener()

	if listener == nil {
		log.Fatalf("Listener is not initialized")
	}

	// Accept incoming connections
	for {
		select {
		case <-lb.stopChan:
			log.Printf("Stop signal received, closing listener for '%s'", lb.listenerAddr)
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Failed to accept connection '%v'", err)
				continue
			}

			go lb.HandleConnection(conn)

		}

	}
}

// HandleConnection handles a single client connection.
func (lb *LoadBalancer) HandleConnection(conn net.Conn) {

	defer conn.Close()

	clientIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		log.Printf("Failed to get client IP: %v", err)
		clientIP = "unknown"
	}

	listenerPort := conn.LocalAddr().(*net.TCPAddr).Port
	log.Printf("New inbound connection from '%s' on listener port '%d'", clientIP, listenerPort)

	lb.mu.Lock()
	// log.Printf("Selecting backend for listener port '%d'", listenerPort)
	backend := lb.getNextBackend()
	lb.mu.Unlock()

	if backend == nil {

		// No healthy backends
		log.Printf("No healthy backends available for client '%s'", clientIP)
		return
	}

	log.Printf("Selected backend configuration '%s', server '%s:%d'", lb.config.Name, backend.IP, backend.Port)
	backend.ActiveConnections++

	defer func() {
		log.Printf("Releasing backend '%s:%d'", backend.IP, backend.Port)
		backend.ActiveConnections--
	}()

	log.Printf("Forwarding request from '%s' to backend '%s:%d' (%s)", clientIP, backend.IP, backend.Port, backend.PortName) // Log the backend port and name
	log.Printf("Dialing backend '%s:%d' with timeout '%s'", backend.IP, backend.Port, lb.requestTimeout)

	// Get a connection from the pool or create a new one
	backendConn, err := net.Dial("tcp", net.JoinHostPort(backend.IP, fmt.Sprintf("%d", backend.Port)))
	if err != nil {

		// Handle backend connection error
		log.Printf("Failed to connect to backend '%s:%d' for client '%s': %v", backend.IP, backend.Port, clientIP, err)

		// Check for specific error types and log accordingly
		if opErr, ok := err.(*net.OpError); ok {
			if opErr.Op == "dial" && opErr.Net == "tcp" {
				log.Printf("Connection refused to backend '%s:%d': %v", backend.IP, backend.Port, opErr.Err)
			} else {
				log.Printf("Network error connecting to backend '%s:%d': %v", backend.IP, backend.Port, opErr.Err)
			}
		}

	}

	// Forward data between client and backend
	log.Printf("Starting to copy data between client '%s' and backend '%s:%d'", clientIP, backend.IP, backend.Port)

	// Use a WaitGroup to wait for both goroutines to finish
	var wg sync.WaitGroup
	wg.Add(2)

	go copyData(backendConn, conn, &wg, "client to backend")
	go copyData(conn, backendConn, &wg, "backend to client")

	// Wait for the data transfer to complete and then return the connection to the pool
	log.Printf("Waiting for data transfer to complete between '%s' and backend '%s:%d'", clientIP, backend.IP, backend.Port)
	defer backendConn.Close()
	log.Printf("Data transfer complete between '%s' and backend '%s:%d'", clientIP, backend.IP, backend.Port)

	wg.Wait()

}

// copyData copies data from src to dst and logs errors.
func copyData(dst net.Conn, src net.Conn, wg *sync.WaitGroup, direction string) {

	defer wg.Done()

	_, err := io.Copy(dst, src)
	if err != nil && err != io.EOF {

		log.Printf("Error copying data '%s': %v", direction, err)

		// Close the destination connection to signal the error
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			closer.CloseWrite()
		}

	}

}

// getNextBackend returns the next backend server (round-robin for now).
func (lb *LoadBalancer) getNextBackend() *backend.BackendServer {

	const maxRetries = 3

	for i := 0; i < maxRetries; i++ {

		if len(lb.backendServers) == 0 {
			return nil
		}

		// Filter backends by listener port
		filteredBackends := []*backend.BackendServer{}

		for _, server := range lb.backendServers {

			// Check if the backend's port name matches the expected configuration
			switch lb.config.Name {

			case "http_traffic_configuration":
				if server.PortName == "http" { // Assuming "http" port name for HTTP backends
					filteredBackends = append(filteredBackends, server)
				}

			case "https_traffic_configuration":
				if server.PortName == "https" { // Assuming "https" port name for HTTPS backends
					filteredBackends = append(filteredBackends, server)
				}

			case "mongodb_internal_service":
				if server.PortName == "mongodb" { // Assuming "mongodb" port name for MongoDB backends
					filteredBackends = append(filteredBackends, server)
				}

			case "rabbitmq_amqp_internal_service":
				if server.PortName == "rabbitmq" { // Assuming "rabbitmq" port name for RabbitMQ backends
					filteredBackends = append(filteredBackends, server)
				}

			default:
				// Handle other cases or log an error if needed
				log.Printf("Unknown backend configuration name '%s'", lb.config.Name)
			}
		}

		if len(filteredBackends) == 0 {
			log.Printf("No healthy backends available for configuration '%s'", lb.config.Name)
			return nil
		}

		// Apply round-robin to the filtered backends
		lb.nextServer = (lb.nextServer + 1) % len(filteredBackends)
		server := filteredBackends[lb.nextServer]

		if server.Healthy {
			return server
		}

		if i < maxRetries-1 {
			time.Sleep(100 * time.Millisecond) // Backoff before retry
		}
	}

	return nil // No healthy backends after retries
}

// StartHealthChecks starts health checks for all backend servers.
func (lb *LoadBalancer) StartHealthChecks() {

	lb.mu.Lock()
	servers := lb.backendServers
	lb.mu.Unlock()

	for _, server := range servers {
		go lb.runHealthCheck(server)
	}

}

func (lb *LoadBalancer) runHealthCheck(server *backend.BackendServer) {

	lb.mu.RLock() // Use RLock for reading healthCheckMap
	key := fmt.Sprintf("%s:%d", server.IP, server.Port)
	if _, ok := lb.healthCheckMap[key]; ok {
		log.Printf("Health check already running for %s", key)
		lb.mu.RUnlock()
		return
	}
	lb.mu.RUnlock()

	lb.mu.Lock() // Use Lock for writing to healthCheckMap and healthCheckCache
	lb.healthCheckMap[key] = true
	lb.healthCheckCache[key] = true // Also protect healthCheckCache
	lb.mu.Unlock()

	log.Printf("Starting health checks with interval '%d' seconds for '%s'", lb.config.HealthCheckInterval, key)

	// No need to check the cache separately, it's always updated together with healthCheckMap

	server.HealthCheck(time.Duration(lb.config.HealthCheckInterval) * time.Second)

}

func (lb *LoadBalancer) StopHealthChecks() {

	log.Printf("Stopping health checks for %s", lb.listenerAddr)
	close(lb.stopHealthChecks)

}

func (lb *LoadBalancer) areHealthChecksStopped() bool {

	select {
	case <-lb.stopHealthChecks:
		return true
	default:
		return false
	}

}

// DiscoverK8sServices discovers services in Kubernetes and adds them as backends.
func (lb *LoadBalancer) DiscoverK8sServices() {

	kubernetes.DiscoverK8sServices(lb, lb.config)

}

// GetMu returns the mutex
func (lb *LoadBalancer) GetMu() *sync.RWMutex {

	return &lb.mu

}

// GetBackendServers returns the backend servers
func (lb *LoadBalancer) GetBackendServers() []*backend.BackendServer {

	return lb.backendServers

}

// SetBackendServers sets the backend servers
func (lb *LoadBalancer) SetBackendServers(servers []*backend.BackendServer) {

	lb.backendServers = servers

}

// GetListener returns the listener
func (lb *LoadBalancer) GetListener() net.Listener {

	return lb.Listener

}

// Stop stops the load balancer
func (lb *LoadBalancer) Stop() {

	if lb.Listener != nil {
		lb.Listener.Close()
		log.Printf("Stopped listening on port: %s", utils.ExtractPort(lb.listenerAddr))
	}

	lb.Listener = nil
	close(lb.stopChan)

	// Wait for health checks to stop
	for !lb.areHealthChecksStopped() {
		time.Sleep(100 * time.Millisecond)
	}

}
