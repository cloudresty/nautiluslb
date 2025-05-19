package backend

import (
	"fmt"
	"log"
	"net"
	"time"
)

// BackendServer represents a backend server.
type BackendServer struct {
	ID                int    `json:"id"`
	IP                string `json:"ip"`
	Port              int    `json:"port"`
	PortName          string `json:"port_name"`
	Weight            int
	ActiveConnections int
	Healthy           bool
	PreviousHealthy   bool // Track previous health status
}

// HealthCheck checks the health of a backend server.
func (server *BackendServer) HealthCheck(interval time.Duration) {

	var lastCheck time.Time

	consecutiveFailures := 0 // Counter for consecutive health check failures

	log.Printf("Starting health checks for %s:%d with interval: %s", server.IP, server.Port, interval)

	for {

		elapsed := time.Since(lastCheck) // Calculate elapsed time since last check
		sleepDuration := interval - elapsed
		time.Sleep(sleepDuration)
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(server.IP, fmt.Sprintf("%d", server.Port)), 2*time.Second)

		healthChanged := false
		if err != nil {

			consecutiveFailures++
			log.Printf("Backend %s:%d is unhealthy (attempt %d): %v", server.IP, server.Port, consecutiveFailures, err)
			if consecutiveFailures >= 3 && server.Healthy { // Require 3 consecutive failures
				server.Healthy = false
				log.Printf("Backend %s:%d is now unhealthy (3 consecutive failures)", server.IP, server.Port)
			}

		} else {

			consecutiveFailures = 0 // Reset failure count on success
			if !server.Healthy {
				server.Healthy = true
				log.Printf("Backend %s:%d is now healthy", server.IP, server.Port)
			}
			conn.Close()

			if !server.Healthy {
				server.Healthy = true
				log.Printf("Backend %s:%d is now healthy", server.IP, server.Port)
			}

		}
		conn.Close()

		if healthChanged {
			log.Printf("Backend %s:%d is %s", server.IP, server.Port, server.healthStatus())
		}
		lastCheck = time.Now()
	}
}
func (server *BackendServer) healthStatus() string {

	if server.Healthy {
		return "healthy"
	}

	return "unhealthy"

}
