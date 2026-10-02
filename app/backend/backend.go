package backend

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cloudresty/emit"
)

// unhealthyThreshold is how many consecutive failed health checks mark a
// backend unhealthy. A single success marks it healthy again.
const unhealthyThreshold = 3

// BackendServer represents a backend server.
//
// Health and the active connection count are read by every connection
// handler and written by the health checker concurrently, so they are atomics
// behind methods rather than plain fields. A BackendServer must therefore not
// be copied after first use.
type BackendServer struct {
	ID       int    `json:"id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	PortName string `json:"port_name"`
	Weight   int

	healthy           atomic.Bool
	activeConnections atomic.Int64
}

// New returns a backend that starts out healthy, so traffic flows before the
// first health check has completed.
func New(id int, ip string, port int, portName string) *BackendServer {
	server := &BackendServer{
		ID:       id,
		IP:       ip,
		Port:     port,
		PortName: portName,
		Weight:   1,
	}
	server.healthy.Store(true)
	return server
}

// Address returns the backend's dialable host:port.
func (server *BackendServer) Address() string {
	return net.JoinHostPort(server.IP, strconv.Itoa(server.Port))
}

// IsHealthy reports the backend's current health.
func (server *BackendServer) IsHealthy() bool {
	return server.healthy.Load()
}

// SetHealthy sets the backend's health and reports whether it changed.
func (server *BackendServer) SetHealthy(healthy bool) bool {
	return server.healthy.Swap(healthy) != healthy
}

// ActiveConnections returns the number of connections currently proxied to
// this backend.
func (server *BackendServer) ActiveConnections() int64 {
	return server.activeConnections.Load()
}

// Acquire records a connection proxied to this backend.
func (server *BackendServer) Acquire() {
	server.activeConnections.Add(1)
}

// Release records the end of a connection proxied to this backend.
func (server *BackendServer) Release() {
	server.activeConnections.Add(-1)
}

// HealthCheck probes the backend with a TCP connect every interval until ctx
// is cancelled. Each probe is bounded by timeout and by ctx.
func (server *BackendServer) HealthCheck(ctx context.Context, interval, timeout time.Duration) {

	failures := 0
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {

		failures = server.checkOnce(ctx, timeout, failures)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

	}

}

// checkOnce runs one probe and returns the updated consecutive-failure count.
func (server *BackendServer) checkOnce(ctx context.Context, timeout time.Duration, failures int) int {

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(probeCtx, "tcp", server.Address())

	if err != nil {

		// A probe cut short by shutdown says nothing about the backend.
		if ctx.Err() != nil {
			return failures
		}

		failures++
		emit.Warn.StructuredFields("Backend health check failed",
			emit.ZString("backend_ip", server.IP),
			emit.ZInt("backend_port", server.Port),
			emit.ZInt("attempt", failures),
			emit.ZString("error", err.Error()))

		if failures >= unhealthyThreshold && server.SetHealthy(false) {
			emit.Error.StructuredFields("Backend marked as unhealthy",
				emit.ZString("backend_ip", server.IP),
				emit.ZInt("backend_port", server.Port),
				emit.ZString("reason", fmt.Sprintf("%d consecutive failures", failures)))
		}

		return failures

	}

	if err := conn.Close(); err != nil {
		emit.Debug.StructuredFields("Failed to close health check connection",
			emit.ZString("backend_ip", server.IP),
			emit.ZInt("backend_port", server.Port),
			emit.ZString("error", err.Error()))
	}

	if server.SetHealthy(true) {
		emit.Info.StructuredFields("Backend recovered to healthy",
			emit.ZString("backend_ip", server.IP),
			emit.ZInt("backend_port", server.Port))
	}

	return 0

}

func (server *BackendServer) healthStatus() string {

	if server.IsHealthy() {
		return "healthy"
	}

	return "unhealthy"

}
