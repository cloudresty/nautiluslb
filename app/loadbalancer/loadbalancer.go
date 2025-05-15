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
	}

	lb.Listener = nil

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
		log.Fatalf("Failed to listen on %s: %v", lb.listenerAddr, err)
	}

	log.Printf("Listening on port: %s", lb.listenerAddr)

	listener := lb.GetListener()

	if listener == nil {
		log.Fatalf("Listener is not initialized")
	}

	// Accept incoming connections
	for {
		select {
		case <-lb.stopChan:
			log.Printf("Stop signal received, closing listener for %s", lb.listenerAddr)
			return
		default:
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Failed to accept connection: %v", err)
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

	// Log the client IP
	log.Printf("New connection from: %s", clientIP)

	lb.mu.Lock()
	backend := lb.getNextBackend()
	lb.mu.Unlock()

	if backend == nil {

		// No healthy backends
		log.Printf("No healthy backends available for client: %s", clientIP)

		return

	}

	backend.ActiveConnections++

	defer func() {
		backend.ActiveConnections--
	}()

	log.Printf("Forwarding request from %s to backend %s:%d (%s)", clientIP, backend.IP, backend.Port, backend.PortName) // Log the backend port and name
	log.Printf("Dialing backend %s:%d with timeout %s", backend.IP, backend.Port, lb.requestTimeout)

	// Use a proxy connection to forward the client connection
	backendConn, err := net.Dial("tcp", net.JoinHostPort(backend.IP, fmt.Sprintf("%d", backend.Port)))
	if err != nil {

		// Handle backend connection error
		log.Printf("Failed to connect to backend %s:%d for client %s: %v", backend.IP, backend.Port, clientIP, err)

		return

	}
	defer backendConn.Close()

	// Forward data between client and backend
	log.Printf("Starting to copy data between client and backend")

	// Use a WaitGroup to wait for both goroutines to finish
	var wg sync.WaitGroup
	wg.Add(2)

	go copyData(backendConn, conn, &wg, "client to backend")
	go copyData(conn, backendConn, &wg, "backend to client")

	wg.Wait()

}

// copyData copies data from src to dst and logs errors.
func copyData(dst net.Conn, src net.Conn, wg *sync.WaitGroup, direction string) {

	defer wg.Done()

	_, err := io.Copy(dst, src)
	if err != nil && err != io.EOF {
		log.Printf("Error copying data %s: %v", direction, err)
	}

}

// getNextBackend returns the next backend server (round-robin for now).
func (lb *LoadBalancer) getNextBackend() *backend.BackendServer {

	if len(lb.backendServers) == 0 {
		return nil
	}

	for range lb.backendServers {

		lb.nextServer = (lb.nextServer + 1) % len(lb.backendServers)
		server := lb.backendServers[lb.nextServer]

		if server.Healthy {
			return server
		}

	}

	return nil

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
		log.Printf("Health check already running for %s:%d", server.IP, server.Port)
		lb.mu.Unlock()
		return
	}

	lb.healthCheckMap[fmt.Sprintf("%s:%d", server.IP, server.Port)] = true
	lb.mu.Unlock()
	log.Printf("Starting health checks with interval: %d seconds for %s:%d", lb.config.HealthCheckInterval, server.IP, server.Port)
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
		log.Printf("Stopped listening on port: %s", lb.listenerAddr)
	}

	lb.Listener = nil
	close(lb.stopChan)

	// Wait for health checks to stop
	for !lb.areHealthChecksStopped() {
		time.Sleep(100 * time.Millisecond)
	}

}
