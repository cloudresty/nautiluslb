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

	failureCounter := 0
	retryLimit := 3
	connectionTimeout := 2 * time.Second

	// log.Printf("Starting health checks for %s:%d with interval: %s", server.IP, server.Port, interval)

	for {

		// Calculate elapsed time since last check
		elapsed := time.Since(lastCheck)
		sleepDuration := interval - elapsed
		time.Sleep(sleepDuration)
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(server.IP, fmt.Sprintf("%d", server.Port)), connectionTimeout)

		healthChanged := false
		if err != nil {

			failureCounter++

			log.Printf("System | Backend %s:%d is unhealthy (attempt %d): %v", server.IP, server.Port, failureCounter, err)

			if failureCounter >= retryLimit && server.Healthy { // Require 3 consecutive failures
				server.Healthy = false
				log.Printf("System | Backend %s:%d is now unhealthy (3 consecutive failures)", server.IP, server.Port)
			}

		} else {

			failureCounter = 0 // Reset failure count on success

			if !server.Healthy {
				server.Healthy = true
				log.Printf("System | Backend %s:%d is now healthy", server.IP, server.Port)
			}
			conn.Close()

			if !server.Healthy {
				server.Healthy = true
				log.Printf("System | Backend %s:%d is now healthy", server.IP, server.Port)
			}

		}
		conn.Close()

		if healthChanged {
			log.Printf("System | Backend %s:%d is %s", server.IP, server.Port, server.healthStatus())
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
