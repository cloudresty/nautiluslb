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
	config           config.Configuration
	requestTimeout   time.Duration
	ListenerAddress  string
}

// NewLoadBalancer creates a new LoadBalancer instance.
func NewLoadBalancer(config config.Configuration, requestTimeout time.Duration) *LoadBalancer {

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
	lb.Listener = nil // This should be after the struct initialization

	return lb
}

// Start starts the load balancer.
func (lb *LoadBalancer) Start() {

	go lb.StartHealthChecks()

	var err error
	lb.Listener, err = net.Listen("tcp", lb.listenerAddr)
	if err != nil {
		log.Fatalf("System | Failed to listen on port '%s': %v", utils.ExtractPort(lb.listenerAddr), err)
	}

	listener := lb.GetListener()

	if listener == nil {
		log.Fatalf("System | Listener is not initialized")
	}

	// Accept incoming connections
	for {

		select {

		case <-lb.stopChan:
			log.Printf("System | Stop signal received, closing listener for '%s'", lb.listenerAddr)
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("System | Failed to accept connection '%v'", err)
				continue
			}

			go lb.HandleConnection(conn)

		}

	}

}

// HandleConnection handles a single client connection.
func (lb *LoadBalancer) HandleConnection(conn net.Conn) {

	defer conn.Close()

	// Get the client IP address
	clientIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		log.Printf("System | Failed to get client IP: %v", err)
		clientIP = "unknown"
	}

	// Get the listener port
	listenerPort := conn.LocalAddr().(*net.TCPAddr).Port
	log.Printf("Client | Received request: %s:%d", clientIP, listenerPort)

	lb.mu.Lock()
	// log.Printf("Selecting backend for listener port '%d'", listenerPort)
	backend := lb.getNextBackend()
	lb.mu.Unlock()

	if backend == nil {

		// No healthy backends
		log.Printf("System | No healthy backends for: %s:%d", clientIP, listenerPort)
		return
	}

	log.Printf("Client | Forwarding traffic: %s:%d -> %s -> %s:%d", clientIP, listenerPort, lb.config.Name, backend.IP, backend.Port)
	backend.ActiveConnections++

	defer func() {
		// log.Printf("Releasing backend '%s:%d'", backend.IP, backend.Port)
		backend.ActiveConnections--
	}()

	// log.Printf("Forwarding request from '%s' to backend '%s:%d' (%s)", clientIP, backend.IP, backend.Port, backend.PortName)
	// log.Printf("Dialing backend '%s:%d' with timeout '%s'", backend.IP, backend.Port, lb.requestTimeout)

	// Get a connection from the pool or create a new one
	backendConn, err := net.Dial("tcp", net.JoinHostPort(backend.IP, fmt.Sprintf("%d", backend.Port)))
	if err != nil {

		// Handle backend connection error
		log.Printf("System | Failed to connect to backend '%s:%d' for client '%s': %v", backend.IP, backend.Port, clientIP, err)

		// Check for specific error types and log accordingly
		if opErr, ok := err.(*net.OpError); ok {
			if opErr.Op == "dial" && opErr.Net == "tcp" {
				log.Printf("System | Connection refused to backend '%s:%d': %v", backend.IP, backend.Port, opErr.Err)
			} else {
				log.Printf("System | Network error connecting to backend '%s:%d': %v", backend.IP, backend.Port, opErr.Err)
			}
		}

	}

	// Forward data between client and backend
	// log.Printf("Starting to copy data between client '%s' and backend '%s:%d'", clientIP, backend.IP, backend.Port)

	// Use a WaitGroup to wait for both goroutines to finish
	var wg sync.WaitGroup
	wg.Add(2)

	go copyData(backendConn, conn, &wg, "client to backend")
	go copyData(conn, backendConn, &wg, "backend to client")

	// Wait for the data transfer to complete and then return the connection to the pool
	// log.Printf("Waiting for data transfer to complete between '%s' and backend '%s:%d'", clientIP, backend.IP, backend.Port)
	defer backendConn.Close()
	// log.Printf("Data transfer complete between '%s' and backend '%s:%d'", clientIP, backend.IP, backend.Port)

	wg.Wait()

}

// copyData copies data from src to dst and logs errors.
func copyData(dst net.Conn, src net.Conn, wg *sync.WaitGroup, direction string) {

	defer wg.Done()

	_, err := io.Copy(dst, src)
	if err != nil && err != io.EOF {

		log.Printf("System | Error copying data '%s': %v", direction, err)

		// Close the destination connection to signal the error
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			closer.CloseWrite()
		}

	}

}

// getNextBackend returns the next backend server (round-robin for now).
func (lb *LoadBalancer) getNextBackend() *backend.BackendServer {

	const maxRetries = 3

	for i := range maxRetries {

		if len(lb.backendServers) == 0 {
			return nil
		}

		// Filter backends by listener port
		filteredBackends := []*backend.BackendServer{}

		for _, server := range lb.backendServers {

			if server.PortName != lb.config.BackendPortName {

				// log.Printf("System | Backend '%s:%d' does not match expected port name '%s'", server.IP, server.Port, lb.config.BackendPortName)
				continue

			} else {

				filteredBackends = append(filteredBackends, server)
				// log.Printf("System | Backend '%s:%d' matches expected port name '%s'", server.IP, server.Port, lb.config.BackendPortName)

			}

		}

		if len(filteredBackends) == 0 {
			log.Printf("System | No healthy backends available for configuration '%s'", lb.config.Name)
			return nil
		}

		// Apply round-robin to the filtered backends
		lb.nextServer = (lb.nextServer + 1) % len(filteredBackends)
		server := filteredBackends[lb.nextServer]

		if server.Healthy {
			return server
		}

		if i < maxRetries-1 {
			time.Sleep(100 * time.Millisecond)
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

	lb.mu.Lock()

	if _, ok := lb.healthCheckMap[fmt.Sprintf("%s:%d", server.IP, server.Port)]; ok {
		log.Printf("System | Health check already running for %s:%d",
			server.IP, server.Port)
		lb.mu.Unlock()
		return
	}

	lb.healthCheckMap[fmt.Sprintf("%s:%d", server.IP, server.Port)] = true
	lb.mu.Unlock()

	// log.Printf("Health check: %s:%d / %ds", server.IP, server.Port, 10)

	// Check if the health check is already in the cache
	if _, exists := lb.healthCheckCache[fmt.Sprintf("%s:%d", server.IP, server.Port)]; !exists {

		log.Printf("System | Health check: %s:%d / %ds", server.IP, server.Port, 10)
		lb.healthCheckCache[fmt.Sprintf("%s:%d", server.IP, server.Port)] = true

	}

	server.HealthCheck(time.Duration(10) * time.Second)

}

// StopHealthChecks stops health checks for all backend servers.
func (lb *LoadBalancer) StopHealthChecks() {

	log.Printf("System | Stopping health checks for %s", lb.listenerAddr)
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
		log.Printf("System | Stopped listening on port: %s", utils.ExtractPort(lb.listenerAddr))
	}

	lb.Listener = nil
	close(lb.stopChan)

	// Wait for health checks to stop
	for !lb.areHealthChecksStopped() {
		time.Sleep(100 * time.Millisecond)
	}

}
