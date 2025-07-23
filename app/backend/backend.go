package backend

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cloudresty/emit"
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

			emit.Warn.StructuredFields("Backend health check failed",
				emit.ZString("backend_ip", server.IP),
				emit.ZInt("backend_port", server.Port),
				emit.ZInt("attempt", failureCounter),
				emit.ZString("error", err.Error()))

			if failureCounter >= retryLimit && server.Healthy { // Require 3 consecutive failures
				server.Healthy = false
				emit.Error.StructuredFields("Backend marked as unhealthy",
					emit.ZString("backend_ip", server.IP),
					emit.ZInt("backend_port", server.Port),
					emit.ZString("reason", "3 consecutive failures"))
			}

		} else {

			failureCounter = 0 // Reset failure count on success

			if !server.Healthy {
				server.Healthy = true
				emit.Info.StructuredFields("Backend recovered to healthy",
					emit.ZString("backend_ip", server.IP),
					emit.ZInt("backend_port", server.Port))
			}
			if err := conn.Close(); err != nil {
				// Only log if it's not an expected "already closed" error
				if !isConnectionClosedError(err) {
					emit.Warn.StructuredFields("Failed to close health check connection",
						emit.ZString("backend_ip", server.IP),
						emit.ZInt("backend_port", server.Port),
						emit.ZString("error", err.Error()))
				}
			}

			if !server.Healthy {
				server.Healthy = true
				emit.Info.StructuredFields("Backend recovered to healthy (duplicate)",
					emit.ZString("backend_ip", server.IP),
					emit.ZInt("backend_port", server.Port))
			}

		}
		if err := conn.Close(); err != nil {
			// Only log if it's not an expected "already closed" error
			if !isConnectionClosedError(err) {
				emit.Warn.StructuredFields("Failed to close health check connection (duplicate)",
					emit.ZString("backend_ip", server.IP),
					emit.ZInt("backend_port", server.Port),
					emit.ZString("error", err.Error()))
			}
		}

		if healthChanged {
			emit.Debug.StructuredFields("Backend health status",
				emit.ZString("backend_ip", server.IP),
				emit.ZInt("backend_port", server.Port),
				emit.ZString("status", server.healthStatus()))
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

// isConnectionClosedError checks if the error is due to connection already being closed
func isConnectionClosedError(err error) bool {
	return strings.Contains(err.Error(), "use of closed network connection")
}
