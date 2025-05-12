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
}

// HealthCheck checks the health of a backend server.
func (server *BackendServer) HealthCheck(interval time.Duration) {

	var lastCheck time.Time

	log.Printf("Starting health checks for %s:%d with interval: %s", server.IP, server.Port, interval)

	for {

		elapsed := time.Since(lastCheck) // Calculate elapsed time since last check
		sleepDuration := interval - elapsed
		time.Sleep(sleepDuration)

		conn, err := net.DialTimeout("tcp", net.JoinHostPort(server.IP, fmt.Sprintf("%d", server.Port)), 2*time.Second)

		if err != nil {

			server.Healthy = false
			log.Printf("Backend %s:%d is unhealthy: %v", server.IP, server.Port, err)

		} else {

			server.Healthy = true
			log.Printf("Backend %s:%d is healthy", server.IP, server.Port)
			conn.Close()

		}

		lastCheck = time.Now()

	}

}
