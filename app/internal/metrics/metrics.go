// Package metrics defines the Recorder used across NautilusLB and its
// Prometheus implementation. No label ever carries a client address or SNI name.
package metrics

import (
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cloudresty/nautiluslb/internal/version"
)

// Recorder is the instrumentation surface; implementations are goroutine-safe.
type Recorder interface {
	ConnAccepted(listener string)
	ConnRejected(listener, reason string)
	ConnActive(listener string, delta int)
	ConnClosed(listener, result string, d time.Duration, in, out int64)
	BackendDial(listener, pool, backend, result string, d time.Duration)
	BackendActive(listener, pool, backend string, delta int)
	BackendHealth(listener, pool, backend string, healthy bool, cause string)
	PoolSize(listener, pool string, healthy, total int)
	ProbeDone(listener, pool, result string, d time.Duration)
	UDPSession(listener, event string)
	UDPDatagram(listener, direction string, n int)
	DiscoveryReconcile(result string, d time.Duration)
	InformerSynced(resource string, ok bool)
	ConfigReload(result string)
	Ready(ok bool)
	PipeMode(mode string)
	AccessLogDropped()
	// DrainForced counts connections force-closed when a drain deadline expired.
	DrainForced(listener string, n int)
}

const ns = "nautiluslb"

type prom struct {
	perBackend bool

	ready, lastReload, lastReconcile prometheus.Gauge
	reload                           *prometheus.CounterVec
	accepted, rejected               *prometheus.CounterVec
	active                           *prometheus.GaugeVec
	connDur                          *prometheus.HistogramVec
	connBytes                        *prometheus.CounterVec
	pipeMode                         *prometheus.CounterVec
	dialTotal                        *prometheus.CounterVec
	dialDur                          *prometheus.HistogramVec
	backendActive, backendHealthy    *prometheus.GaugeVec
	transitions                      *prometheus.CounterVec
	poolBackends                     *prometheus.GaugeVec
	probeDur                         *prometheus.HistogramVec
	udpActive                        *prometheus.GaugeVec
	udpSessions, udpDatagrams        *prometheus.CounterVec
	reconcileTotal                   *prometheus.CounterVec
	reconcileDur                     prometheus.Observer
	informerSynced                   *prometheus.GaugeVec
	dropped                          prometheus.Counter
	drainForced                      *prometheus.CounterVec
}

func lbl(perBackend bool, l ...string) []string {
	if perBackend {
		// backend sits after listener, pool when present.
		out := append([]string{}, l[:2]...)
		out = append(out, "backend")
		return append(out, l[2:]...)
	}
	return l
}

// NewPrometheus registers every catalogue metric on reg. With perBackend
// false, backend-labelled counters/gauges drop the backend label (aggregated
// per pool) and backend_healthy is not registered.
func NewPrometheus(reg prometheus.Registerer, perBackend bool) Recorder {
	p := &prom{perBackend: perBackend}
	f := promauto(reg)
	p.ready = f.gauge("ready", "1 when /readyz is 200.")
	p.lastReload = f.gauge("config_last_reload_timestamp_seconds", "Unix time of the last applied config reload.")
	p.reload = f.counter("config_reload_total", "Config reload outcomes.", "result")
	p.accepted = f.counter("connections_accepted_total", "Connections accepted after ACL and limits.", "listener")
	p.rejected = f.counter("connections_rejected_total", "Connections closed before proxying.", "listener", "reason")
	p.active = f.gaugeVec("connections_active", "Registered connections.", "listener")
	p.connDur = f.hist("connection_duration_seconds", "Connection duration at close.",
		prometheus.ExponentialBucketsRange(0.01, 3600, 16), "listener", "result")
	p.connBytes = f.counter("connection_bytes_total", "Proxied bytes.", "listener", "direction")
	p.pipeMode = f.counter("pipe_mode_total", "Copy path used.", "mode")
	p.dialTotal = f.counter("backend_dial_total", "Backend dial outcomes.", lbl(perBackend, "listener", "pool", "result")...)
	p.dialDur = f.hist("backend_dial_duration_seconds", "Backend connect latency.",
		prometheus.ExponentialBucketsRange(0.001, 10, 14), "listener", "pool")
	p.backendActive = f.gaugeVec("backend_connections_active", "In-flight connections.", lbl(perBackend, "listener", "pool")...)
	if perBackend {
		p.backendHealthy = f.gaugeVec("backend_healthy", "1 healthy, 0 unhealthy.", "listener", "pool", "backend")
	}
	p.transitions = f.counter("backend_health_transitions_total", "Backend health transitions.", lbl(perBackend, "listener", "pool", "to", "cause")...)
	p.poolBackends = f.gaugeVec("pool_backends", "Pool size by state.", "listener", "pool", "state")
	p.probeDur = f.hist("health_probe_duration_seconds", "Probe latency.",
		prometheus.ExponentialBucketsRange(0.001, 10, 14), "listener", "pool", "result")
	p.udpActive = f.gaugeVec("udp_sessions_active", "Active UDP sessions.", "listener")
	p.udpSessions = f.counter("udp_sessions_total", "UDP session events.", "listener", "event")
	p.udpDatagrams = f.counter("udp_datagrams_total", "UDP datagrams.", "listener", "direction")
	p.reconcileTotal = f.counter("discovery_reconcile_total", "Discovery reconcile outcomes.", "result")
	p.reconcileDur = f.hist("discovery_reconcile_duration_seconds", "Reconcile duration.",
		prometheus.ExponentialBucketsRange(0.001, 30, 14)).WithLabelValues()
	p.lastReconcile = f.gauge("discovery_last_success_timestamp_seconds", "Unix time of the last successful reconcile.")
	p.informerSynced = f.gaugeVec("discovery_informer_synced", "1 when informers of the resource are synced.", "resource")
	p.drainForced = f.counter("drain_forced_total", "Connections force-closed when a drain deadline expired.", "listener")
	p.dropped = f.counter("accesslog_dropped_total", "Access log records dropped on overflow.").WithLabelValues()
	return p
}

// factory registers with the nautiluslb_ prefix; duplicate registration panics.
type factory struct{ reg prometheus.Registerer }

func promauto(reg prometheus.Registerer) factory { return factory{reg} }

func (f factory) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help})
	f.reg.MustRegister(g)
	return g
}

func (f factory) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help}, labels)
	f.reg.MustRegister(g)
	return g
}

func (f factory) counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help}, labels)
	f.reg.MustRegister(c)
	return c
}

func (f factory) hist(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(h)
	return h
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (p *prom) ConnAccepted(l string)          { p.accepted.WithLabelValues(l).Inc() }
func (p *prom) ConnRejected(l, reason string)  { p.rejected.WithLabelValues(l, reason).Inc() }
func (p *prom) ConnActive(l string, delta int) { p.active.WithLabelValues(l).Add(float64(delta)) }

func (p *prom) ConnClosed(l, result string, d time.Duration, in, out int64) {
	p.connDur.WithLabelValues(l, result).Observe(d.Seconds())
	if in > 0 {
		p.connBytes.WithLabelValues(l, "in").Add(float64(in))
	}
	if out > 0 {
		p.connBytes.WithLabelValues(l, "out").Add(float64(out))
	}
}

func (p *prom) BackendDial(l, pool, backend, result string, d time.Duration) {
	if p.perBackend {
		p.dialTotal.WithLabelValues(l, pool, backend, result).Inc()
	} else {
		p.dialTotal.WithLabelValues(l, pool, result).Inc()
	}
	p.dialDur.WithLabelValues(l, pool).Observe(d.Seconds())
}

func (p *prom) BackendActive(l, pool, backend string, delta int) {
	if p.perBackend {
		p.backendActive.WithLabelValues(l, pool, backend).Add(float64(delta))
	} else {
		p.backendActive.WithLabelValues(l, pool).Add(float64(delta))
	}
}

func (p *prom) BackendHealth(l, pool, backend string, healthy bool, cause string) {
	to := "unhealthy"
	if healthy {
		to = "healthy"
	}
	if p.perBackend {
		p.backendHealthy.WithLabelValues(l, pool, backend).Set(b2f(healthy))
		p.transitions.WithLabelValues(l, pool, backend, to, cause).Inc()
	} else {
		p.transitions.WithLabelValues(l, pool, to, cause).Inc()
	}
}

func (p *prom) PoolSize(l, pool string, healthy, total int) {
	p.poolBackends.WithLabelValues(l, pool, "healthy").Set(float64(healthy))
	p.poolBackends.WithLabelValues(l, pool, "unhealthy").Set(float64(total - healthy))
}

func (p *prom) ProbeDone(l, pool, result string, d time.Duration) {
	p.probeDur.WithLabelValues(l, pool, result).Observe(d.Seconds())
}

// UDPSession: "open" raises the active gauge; "expired" and "error" lower it.
func (p *prom) UDPSession(l, event string) {
	p.udpSessions.WithLabelValues(l, event).Inc()
	switch event {
	case "open":
		p.udpActive.WithLabelValues(l).Inc()
	case "expired", "error":
		p.udpActive.WithLabelValues(l).Dec()
	}
}

func (p *prom) UDPDatagram(l, direction string, n int) {
	p.udpDatagrams.WithLabelValues(l, direction).Inc()
}

func (p *prom) DiscoveryReconcile(result string, d time.Duration) {
	p.reconcileTotal.WithLabelValues(result).Inc()
	p.reconcileDur.Observe(d.Seconds())
	if result == "applied" || result == "unchanged" {
		p.lastReconcile.SetToCurrentTime()
	}
}

func (p *prom) InformerSynced(resource string, ok bool) {
	p.informerSynced.WithLabelValues(resource).Set(b2f(ok))
}

func (p *prom) ConfigReload(result string) {
	p.reload.WithLabelValues(result).Inc()
	if result == "applied" {
		p.lastReload.SetToCurrentTime()
	}
}

func (p *prom) Ready(ok bool)        { p.ready.Set(b2f(ok)) }
func (p *prom) PipeMode(mode string) { p.pipeMode.WithLabelValues(mode).Inc() }
func (p *prom) AccessLogDropped()    { p.dropped.Inc() }

func (p *prom) DrainForced(l string, n int) {
	if n > 0 {
		p.drainForced.WithLabelValues(l).Add(float64(n))
	}
}

type nop struct{}

func (nop) ConnAccepted(string)                                       {}
func (nop) ConnRejected(string, string)                               {}
func (nop) ConnActive(string, int)                                    {}
func (nop) ConnClosed(string, string, time.Duration, int64, int64)    {}
func (nop) BackendDial(string, string, string, string, time.Duration) {}
func (nop) BackendActive(string, string, string, int)                 {}
func (nop) BackendHealth(string, string, string, bool, string)        {}
func (nop) PoolSize(string, string, int, int)                         {}
func (nop) ProbeDone(string, string, string, time.Duration)           {}
func (nop) UDPSession(string, string)                                 {}
func (nop) UDPDatagram(string, string, int)                           {}
func (nop) DiscoveryReconcile(string, time.Duration)                  {}
func (nop) InformerSynced(string, bool)                               {}
func (nop) ConfigReload(string)                                       {}
func (nop) Ready(bool)                                                {}
func (nop) PipeMode(string)                                           {}
func (nop) AccessLogDropped()                                         {}
func (nop) DrainForced(string, int)                                   {}

// NewNop returns a Recorder that discards everything.
func NewNop() Recorder { return nop{} }

// Handler serves g in Prometheus text or OpenMetrics format.
func Handler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// RegisterBuildInfo registers nautiluslb_build_info (constant 1).
func RegisterBuildInfo(reg prometheus.Registerer) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "build_info", Help: "Build identity.",
		ConstLabels: prometheus.Labels{
			"version": version.Version, "commit": version.Commit, "go_version": runtime.Version(),
		},
	})
	g.Set(1)
	reg.MustRegister(g)
}

// NewRegistry returns a registry with the Go and process collectors installed.
func NewRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}
