package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/backend"
	"github.com/cloudresty/nautiluslb/config"
	"github.com/cloudresty/nautiluslb/utils"
)

const (
	// maxDialAttempts is the retry budget for one client connection: how many
	// distinct backends are tried before the client is turned away.
	maxDialAttempts = 3

	// defaultDialTimeout applies when requestTimeout is not configured, and
	// maxDialTimeout caps it. requestTimeout bounds connecting to a backend,
	// never the life of the connection: the listeners carry websockets, SSE,
	// MongoDB and Redis sessions that legitimately stay open for hours.
	defaultDialTimeout = 5 * time.Second
	maxDialTimeout     = 10 * time.Second

	// halfCloseIdleTimeout bounds a connection after one side has finished
	// sending. Without it, a peer that never closes its half leaks the
	// connection and both goroutines for ever. It is an idle timeout, reset
	// by every byte, so a long response after a half-close still completes.
	halfCloseIdleTimeout = 2 * time.Minute

	// writeStallTimeout bounds one write to a peer that has stopped reading.
	// It applies only while there is data to deliver, so an idle connection
	// is never affected.
	writeStallTimeout = 2 * time.Minute

	healthCheckInterval = 10 * time.Second
	healthCheckTimeout  = 2 * time.Second

	copyBufferSize = 32 * 1024
)

// keepAlive detects a peer that vanished without closing (a rebooted node, a
// dropped VPN session). It is what bounds an idle long-lived connection,
// without ever closing one whose peer is still there. A dead peer is noticed
// after Idle + Interval*Count = 60s.
var keepAlive = net.KeepAliveConfig{
	Enable:   true,
	Idle:     30 * time.Second,
	Interval: 10 * time.Second,
	Count:    3,
}

// LoadBalancer represents the load balancer.
type LoadBalancer struct {
	config          config.Configuration
	requestTimeout  time.Duration
	dialTimeout     time.Duration
	ListenerAddress string

	mu             sync.RWMutex
	backendServers []*backend.BackendServer
	nextServer     int
	listener       net.Listener

	// Health checks run only between Start and Stop. healthCtx is nil
	// outside that window, so backends set before Start (and in tests) do
	// not spawn probes.
	healthCtx    context.Context
	healthCancel context.CancelFunc
	healthChecks map[*backend.BackendServer]context.CancelFunc
	healthWG     sync.WaitGroup

	stopChan chan struct{}
	stopOnce sync.Once

	// dial is net.Dialer.DialContext in production and replaceable in tests.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewLoadBalancer creates a new LoadBalancer instance.
func NewLoadBalancer(config config.Configuration, requestTimeout time.Duration) *LoadBalancer {

	dialer := &net.Dialer{KeepAliveConfig: keepAlive}

	return &LoadBalancer{
		config:          config,
		requestTimeout:  requestTimeout,
		dialTimeout:     dialTimeoutFor(requestTimeout),
		ListenerAddress: config.ListenerAddress,
		backendServers:  []*backend.BackendServer{},
		healthChecks:    make(map[*backend.BackendServer]context.CancelFunc),
		stopChan:        make(chan struct{}),
		dial:            dialer.DialContext,
	}

}

func dialTimeoutFor(requestTimeout time.Duration) time.Duration {

	switch {
	case requestTimeout <= 0:
		return defaultDialTimeout
	case requestTimeout > maxDialTimeout:
		return maxDialTimeout
	default:
		return requestTimeout
	}

}

// Listen binds the listener and starts health checks, without serving. It
// returns the bind error (typically the port is in use) so the caller can
// fail startup cleanly. After Stop it binds nothing and returns nil.
func (lb *LoadBalancer) Listen() error {

	listenConfig := net.ListenConfig{KeepAliveConfig: keepAlive}
	listener, err := listenConfig.Listen(context.Background(), "tcp", lb.ListenerAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", lb.ListenerAddress, err)
	}

	if !lb.begin(listener) {
		_ = listener.Close()
	}

	return nil

}

// Start serves the listener bound by Listen until Stop is called. It does
// nothing if Listen bound none (Stop ran first).
func (lb *LoadBalancer) Start() {

	if listener := lb.GetListener(); listener != nil {
		lb.Serve(listener)
	}

}

// begin records the listener and starts health checks, unless Stop already
// ran.
func (lb *LoadBalancer) begin(listener net.Listener) bool {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	select {
	case <-lb.stopChan:
		return false
	default:
	}

	lb.listener = listener
	lb.healthCtx, lb.healthCancel = context.WithCancel(context.Background())
	lb.reconcileHealthChecksLocked()

	return true

}

// Serve accepts connections on listener until it is closed.
func (lb *LoadBalancer) Serve(listener net.Listener) {

	var backoff time.Duration

	for {

		conn, err := listener.Accept()
		if err != nil {

			if errors.Is(err, net.ErrClosed) {
				emit.Info.StructuredFields("Listener closed",
					emit.ZString("listener_addr", lb.ListenerAddress))
				return
			}

			// Typically EMFILE. Back off rather than spin on the error.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			emit.Error.StructuredFields("Failed to accept connection",
				emit.ZString("error", err.Error()),
				emit.ZString("retry_in", backoff.String()))

			select {
			case <-lb.stopChan:
				return
			case <-time.After(backoff):
			}

			continue

		}

		backoff = 0
		go lb.HandleConnection(conn)

	}

}

// HandleConnection proxies one client connection to a backend. It always
// closes conn, and a panic anywhere in it is contained to this connection.
func (lb *LoadBalancer) HandleConnection(conn net.Conn) {

	defer closeConn(conn, "client")
	defer recoverConnection("handle connection", conn)

	clientIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		clientIP = "unknown"
	}

	listenerPort := 0
	if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		listenerPort = addr.Port
	}

	emit.Info.StructuredFields("Received client request",
		emit.ZString("client_ip", clientIP),
		emit.ZInt("listener_port", listenerPort))

	server, backendConn := lb.connectBackend(clientIP, listenerPort)
	if backendConn == nil {
		return
	}

	defer closeConn(backendConn, "backend")

	server.Acquire()
	defer server.Release()

	emit.Info.StructuredFields("Forwarding client traffic to backend",
		emit.ZString("client_ip", clientIP),
		emit.ZInt("listener_port", listenerPort),
		emit.ZString("loadbalancer", lb.config.Name),
		emit.ZString("backend_ip", server.IP),
		emit.ZInt("backend_port", server.Port))

	proxy(conn, backendConn)

}

// connectBackend dials up to maxDialAttempts distinct backends. It returns a
// nil connection only when every attempt failed, and the caller must then
// close the client rather than proxy.
func (lb *LoadBalancer) connectBackend(clientIP string, listenerPort int) (*backend.BackendServer, net.Conn) {

	candidates := lb.nextBackends(maxDialAttempts)
	if len(candidates) == 0 {
		emit.Error.StructuredFields("No backends available",
			emit.ZString("client_ip", clientIP),
			emit.ZInt("listener_port", listenerPort),
			emit.ZString("loadbalancer", lb.config.Name))
		return nil, nil
	}

	for attempt, server := range candidates {

		ctx, cancel := context.WithTimeout(context.Background(), lb.dialTimeout)
		backendConn, err := lb.dial(ctx, "tcp", server.Address())
		cancel()

		if err == nil {
			return server, backendConn
		}

		// Eject at once rather than wait for three failed health checks;
		// the health checker restores it on its next successful probe.
		ejected := server.SetHealthy(false)

		emit.Warn.StructuredFields("Failed to connect to backend",
			emit.ZString("client_ip", clientIP),
			emit.ZString("loadbalancer", lb.config.Name),
			emit.ZString("backend_ip", server.IP),
			emit.ZInt("backend_port", server.Port),
			emit.ZInt("attempt", attempt+1),
			emit.ZInt("attempts_allowed", len(candidates)),
			emit.ZBool("ejected", ejected),
			emit.ZString("error", err.Error()))

	}

	emit.Error.StructuredFields("All backend connection attempts failed, closing client connection",
		emit.ZString("client_ip", clientIP),
		emit.ZInt("listener_port", listenerPort),
		emit.ZString("loadbalancer", lb.config.Name),
		emit.ZInt("attempts", len(candidates)))

	return nil, nil

}

// nextBackends returns up to n distinct backends in round-robin order,
// healthy ones only. If none is healthy it falls back to all of them: a
// health check can be wrong (and passive ejection is aggressive), and trying
// a backend costs one dial, where refusing outright turns a false negative
// into an outage.
func (lb *LoadBalancer) nextBackends(n int) []*backend.BackendServer {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	var all, healthy []*backend.BackendServer
	for _, server := range lb.backendServers {
		if server.PortName != lb.config.BackendPortName {
			continue
		}
		all = append(all, server)
		if server.IsHealthy() {
			healthy = append(healthy, server)
		}
	}

	pool := healthy
	if len(pool) == 0 {
		if len(all) > 0 {
			emit.Warn.StructuredFields("No healthy backends, trying unhealthy ones",
				emit.ZString("loadbalancer", lb.config.Name),
				emit.ZInt("backends", len(all)))
		}
		pool = all
	}

	if len(pool) == 0 {
		return nil
	}

	lb.nextServer = (lb.nextServer + 1) % len(pool)

	count := min(n, len(pool))
	picked := make([]*backend.BackendServer, 0, count)
	for i := range count {
		picked = append(picked, pool[(lb.nextServer+i)%len(pool)])
	}

	return picked

}

// getNextBackend returns the next backend server (round-robin).
func (lb *LoadBalancer) getNextBackend() *backend.BackendServer {

	picked := lb.nextBackends(1)
	if len(picked) == 0 {
		return nil
	}

	return picked[0]

}

// proxy copies both directions until both have finished, then returns. The
// caller owns closing both connections.
//
// A clean EOF in one direction is forwarded as a half-close (CloseWrite), so
// protocols that half-close keep working. Any error aborts both directions by
// closing both connections, which unblocks the other copy at once. After a
// half-close the remaining direction runs under an idle timeout, so a peer
// that never finishes cannot hold the connection for ever.
func proxy(client, upstream net.Conn) {

	var abortOnce sync.Once
	abort := func() {
		abortOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}

	var halfClosed atomic.Bool

	toUpstream := &pipe{dst: upstream, src: client, direction: "client to backend", halfClosed: &halfClosed, abort: abort}
	toClient := &pipe{dst: client, src: upstream, direction: "backend to client", halfClosed: &halfClosed, abort: abort}

	done := make(chan struct{})
	go func() {
		defer close(done)
		toUpstream.run()
	}()

	toClient.run()
	<-done

}

type pipe struct {
	dst, src   net.Conn
	direction  string
	halfClosed *atomic.Bool
	abort      func()
}

func (p *pipe) run() {

	defer func() {
		if r := recover(); r != nil {
			emit.Error.StructuredFields("Recovered panic while copying data",
				emit.ZString("direction", p.direction),
				emit.ZString("panic", fmt.Sprint(r)),
				emit.ZString("stack", string(debug.Stack())))
			p.abort()
		}
	}()

	reader := &idleReader{conn: p.src, halfClosed: p.halfClosed}
	writer := &stallWriter{conn: p.dst}

	_, err := io.CopyBuffer(writer, reader, make([]byte, copyBufferSize))
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			emit.Warn.StructuredFields("Connection aborted",
				emit.ZString("direction", p.direction),
				emit.ZString("error", err.Error()))
		}
		p.abort()
		return
	}

	// Clean EOF: pass the half-close on, then bound the other direction.
	p.halfClosed.Store(true)
	if err := closeWrite(p.dst); err != nil {
		p.abort()
		return
	}
	_ = p.dst.SetReadDeadline(time.Now().Add(halfCloseIdleTimeout))

}

// idleReader applies halfCloseIdleTimeout to every read once either side has
// half-closed. Before that, reads have no deadline at all.
type idleReader struct {
	conn       net.Conn
	halfClosed *atomic.Bool
}

func (r *idleReader) Read(b []byte) (int, error) {

	if r.halfClosed.Load() {
		_ = r.conn.SetReadDeadline(time.Now().Add(halfCloseIdleTimeout))
	}

	return r.conn.Read(b)

}

// stallWriter bounds each write by writeStallTimeout.
type stallWriter struct {
	conn net.Conn
}

func (w *stallWriter) Write(b []byte) (int, error) {

	_ = w.conn.SetWriteDeadline(time.Now().Add(writeStallTimeout))
	return w.conn.Write(b)

}

func closeWrite(conn net.Conn) error {

	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}

	// No half-close available: closing is the only way to signal EOF.
	return conn.Close()

}

func closeConn(conn net.Conn, which string) {

	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		emit.Debug.StructuredFields("Failed to close connection",
			emit.ZString("connection", which),
			emit.ZString("error", err.Error()))
	}

}

// recoverConnection contains a panic to the connection that raised it. A
// panic in any goroutine kills the whole process, and with it every other
// connection on every listener.
func recoverConnection(where string, conn net.Conn) {

	if r := recover(); r != nil {
		emit.Error.StructuredFields("Recovered panic in connection handler",
			emit.ZString("where", where),
			emit.ZString("client", conn.RemoteAddr().String()),
			emit.ZString("panic", fmt.Sprint(r)),
			emit.ZString("stack", string(debug.Stack())))
	}

}

// StartHealthChecks reconciles health checks with the current backends: one
// probe loop per backend, started for new ones and stopped for removed ones.
// It does nothing before Start or after Stop.
func (lb *LoadBalancer) StartHealthChecks() {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.reconcileHealthChecksLocked()

}

func (lb *LoadBalancer) reconcileHealthChecksLocked() {

	if lb.healthCtx == nil || lb.healthCtx.Err() != nil {
		return
	}

	current := make(map[*backend.BackendServer]bool, len(lb.backendServers))
	for _, server := range lb.backendServers {
		current[server] = true
	}

	for server, cancel := range lb.healthChecks {
		if !current[server] {
			cancel()
			delete(lb.healthChecks, server)
		}
	}

	for server := range current {

		if _, running := lb.healthChecks[server]; running {
			continue
		}

		ctx, cancel := context.WithCancel(lb.healthCtx)
		lb.healthChecks[server] = cancel
		lb.healthWG.Add(1)

		emit.Info.StructuredFields("Starting health check for backend",
			emit.ZString("loadbalancer", lb.config.Name),
			emit.ZString("backend_ip", server.IP),
			emit.ZInt("backend_port", server.Port),
			emit.ZInt("interval_seconds", int(healthCheckInterval/time.Second)))

		go func() {
			defer lb.healthWG.Done()
			defer func() {
				if r := recover(); r != nil {
					emit.Error.StructuredFields("Recovered panic in health check",
						emit.ZString("backend", server.Address()),
						emit.ZString("panic", fmt.Sprint(r)),
						emit.ZString("stack", string(debug.Stack())))
				}
			}()
			server.HealthCheck(ctx, healthCheckInterval, healthCheckTimeout)
		}()

	}

}

// StopHealthChecks stops every health check and waits for them to exit.
func (lb *LoadBalancer) StopHealthChecks() {

	lb.mu.Lock()
	if lb.healthCancel != nil {
		lb.healthCancel()
	}
	clear(lb.healthChecks)
	lb.mu.Unlock()

	lb.healthWG.Wait()

}

// GetBackendServers returns a snapshot of the backend servers.
func (lb *LoadBalancer) GetBackendServers() []*backend.BackendServer {

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	return append([]*backend.BackendServer{}, lb.backendServers...)

}

// SetBackendServers replaces the backend servers.
//
// A backend whose address is already known keeps its existing object, and
// with it its health state, connection count and running health check. A
// rediscovered backend must not be reset to healthy, nor left without a
// health check while an orphaned probe updates an object nobody reads.
func (lb *LoadBalancer) SetBackendServers(servers []*backend.BackendServer) {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	existing := make(map[string]*backend.BackendServer, len(lb.backendServers))
	for _, server := range lb.backendServers {
		existing[server.Address()+"/"+server.PortName] = server
	}

	merged := make([]*backend.BackendServer, 0, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		if known, ok := existing[server.Address()+"/"+server.PortName]; ok {
			server = known
		}
		merged = append(merged, server)
	}

	lb.backendServers = merged
	lb.reconcileHealthChecksLocked()

}

// GetListener returns the listener, or nil before Start.
func (lb *LoadBalancer) GetListener() net.Listener {

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	return lb.listener

}

// Stop closes the listener and stops health checks. Connections already
// being proxied are left to finish. It is safe to call more than once.
func (lb *LoadBalancer) Stop() {

	lb.stopOnce.Do(func() {

		lb.mu.Lock()
		close(lb.stopChan)
		listener := lb.listener
		lb.mu.Unlock()

		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				emit.Warn.StructuredFields("Failed to close listener",
					emit.ZString("error", err.Error()))
			}
			emit.Info.StructuredFields("Stopped listening on port",
				emit.ZString("port", utils.ExtractPort(lb.ListenerAddress)))
		}

		lb.StopHealthChecks()

	})

}
