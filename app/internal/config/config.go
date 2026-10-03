// Package config loads, defaults and validates the NautilusLB configuration.
//
// Pipeline: Load = read + Parse (strict) + ApplyDefaults + ApplyEnv + Validate.
//
// Defaults that are true (settings.discovery.nodes.readyOnly,
// skipUnschedulable, settings.accessLog.enabled, settings.admin.metrics.perBackend,
// and health.jitter 0.2) must stay distinguishable from an explicit false/0, so
// the public fields are plain bool/float64 and Parse pre-seeds them before
// decoding: an absent key keeps the default, an explicit value overrides it.
// For a hand-built Config (never parsed) ApplyDefaults seeds them the first time
// it runs, tracked by an unexported marker; set an explicit false after
// ApplyDefaults. Per-route health overrides cannot express jitter 0 (zero
// inherits the configuration's value).
package config

import (
	"regexp"
	"time"
)

const (
	APIVersion = "nautiluslb.cloudresty.io/v1"
	Kind       = "Config"

	// ServiceEnabledAnnotation marks a Service as a NautilusLB backend.
	ServiceEnabledAnnotation = "nautiluslb.cloudresty.io/enabled"
	// ServiceConfigurationsAnnotation binds a Service to configurations by name
	// (or "<config>/<route>" for an SNI route), comma-separated. A Service is a
	// backend only when it names the configuration here: matching on port name
	// alone would let any annotated Service join a listener's pool.
	ServiceConfigurationsAnnotation = "nautiluslb.cloudresty.io/configurations"
	// ServiceWeightAnnotation sets the weight (1..100, default 1) of a Service's endpoints.
	ServiceWeightAnnotation = "nautiluslb.cloudresty.io/weight"
	// AllNamespaces in Namespaces opts a configuration into cluster-wide
	// discovery. It must be the only entry.
	AllNamespaces = "*"

	// MaxDialTimeout caps Configuration.DialTimeout (see EffectiveDialTimeout).
	MaxDialTimeout = 10 * time.Second
)

// namePattern restricts configuration and route names: they are referenced
// inside a comma-separated annotation, so commas, spaces and '/' are impossible.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
	ProtocolTLS Protocol = "tls"
)

// Config is the whole configuration file.
type Config struct {
	APIVersion     string          `yaml:"apiVersion"`
	Kind           string          `yaml:"kind"`
	Settings       Settings        `yaml:"settings"`
	Configurations []Configuration `yaml:"configurations"`

	seeded       bool     // true-valued bool defaults already applied/seeded
	parseWarning []string // deprecations found while parsing (bare-int durations)
}

type Settings struct {
	LogLevel   string             `yaml:"logLevel"`
	Kubernetes KubernetesSettings `yaml:"kubernetes"`
	Discovery  DiscoverySettings  `yaml:"discovery"`
	Admin      AdminSettings      `yaml:"admin"`
	AccessLog  AccessLogSettings  `yaml:"accessLog"`
	Limits     GlobalLimits       `yaml:"limits"`
	Drain      DrainSettings      `yaml:"drain"`
	Reload     ReloadSettings     `yaml:"reload"`

	// Deprecated v0.x alias for Kubernetes.Kubeconfig.
	KubeconfigPath string `yaml:"kubeconfigPath,omitempty"`
}

type KubernetesSettings struct {
	Kubeconfig string  `yaml:"kubeconfig"`
	QPS        float32 `yaml:"qps"`
	Burst      int     `yaml:"burst"`
	UserAgent  string  `yaml:"userAgent"`
}

type DiscoverySettings struct {
	ResyncPeriod Duration   `yaml:"resyncPeriod"`
	Debounce     Duration   `yaml:"debounce"`
	Nodes        NodeFilter `yaml:"nodes"`
}

type NodeFilter struct {
	ReadyOnly         bool              `yaml:"readyOnly"`
	SkipUnschedulable bool              `yaml:"skipUnschedulable"`
	SkipControlPlane  bool              `yaml:"skipControlPlane"`
	AddressFamily     string            `yaml:"addressFamily"`
	Selector          map[string]string `yaml:"selector"`
}

type AdminMetrics struct {
	PerBackend bool `yaml:"perBackend"`
}

type AdminSettings struct {
	Address string       `yaml:"address"`
	Pprof   bool         `yaml:"pprof"`
	Metrics AdminMetrics `yaml:"metrics"`
}

type AccessLogSettings struct {
	Enabled    bool   `yaml:"enabled"`
	Output     string `yaml:"output"`
	BufferSize int    `yaml:"bufferSize"`
}

type GlobalLimits struct {
	MaxConnections int `yaml:"maxConnections"`
}

type DrainSettings struct {
	ReadinessDelay Duration `yaml:"readinessDelay"`
	Timeout        Duration `yaml:"timeout"`
}

type ReloadSettings struct {
	WatchFile bool `yaml:"watchFile"`
}

// Configuration is one listener (tcp/udp) or one SNI-routing listener (tls).
type Configuration struct {
	Name            string         `yaml:"name"`
	Protocol        Protocol       `yaml:"protocol"`
	ListenerAddress string         `yaml:"listenerAddress"`
	Namespaces      []string       `yaml:"namespaces"`
	Namespace       string         `yaml:"namespace,omitempty"` // deprecated alias, merged with Namespaces
	BackendPortName string         `yaml:"backendPortName"`
	Balancer        Balancer       `yaml:"balancer"`
	Health          Health         `yaml:"health"`
	Limits          ListenerLimits `yaml:"limits"`
	Access          Access         `yaml:"access"`
	ProxyProtocol   ProxyProtocol  `yaml:"proxyProtocol"`
	DialTimeout     Duration       `yaml:"dialTimeout"`
	RequestTimeout  int            `yaml:"requestTimeout,omitempty"` // deprecated alias (seconds) -> DialTimeout
	IdleTimeout     Duration       `yaml:"idleTimeout"`
	TLS             *TLS           `yaml:"tls,omitempty"`
	UDP             *UDP           `yaml:"udp,omitempty"`

	seeded bool // jitter default already seeded/applied
}

type Balancer struct {
	Algorithm string   `yaml:"algorithm"`
	SlowStart Duration `yaml:"slowStart"`
}

type Health struct {
	Type         string    `yaml:"type"`
	Interval     Duration  `yaml:"interval"`
	Timeout      Duration  `yaml:"timeout"`
	EjectionHold Duration  `yaml:"ejectionHold"`
	Rise         int       `yaml:"rise"`
	Fall         int       `yaml:"fall"`
	Jitter       float64   `yaml:"jitter"`
	Port         int       `yaml:"port"`
	HTTP         HTTPProbe `yaml:"http"`
}

type HTTPProbe struct {
	Path         string `yaml:"path"`
	Host         string `yaml:"host"`
	ExpectStatus []int  `yaml:"expectStatus"`
}

type ListenerLimits struct {
	MaxConnections           int `yaml:"maxConnections"`
	MaxConnectionsPerSource  int `yaml:"maxConnectionsPerSource"`
	MaxConnectionsPerBackend int `yaml:"maxConnectionsPerBackend"`
}

type Access struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

type ProxyProtocolIn struct {
	TrustedCIDRs []string `yaml:"trustedCIDRs"`
	Required     bool     `yaml:"required"`
}

type ProxyProtocol struct {
	In  ProxyProtocolIn `yaml:"in"`
	Out string          `yaml:"out"`
}

type TLS struct {
	PeekTimeout    Duration `yaml:"peekTimeout"`
	MaxClientHello int      `yaml:"maxClientHello"`
	DefaultRoute   string   `yaml:"defaultRoute"`
	Routes         []Route  `yaml:"routes"`
}

// Route is one SNI route. Balancer, Health and Limits are optional overrides
// merged field-wise over the configuration's (zero values inherit).
type Route struct {
	Name            string          `yaml:"name"`
	Hosts           []string        `yaml:"hosts"`
	BackendPortName string          `yaml:"backendPortName"`
	Balancer        *Balancer       `yaml:"balancer,omitempty"`
	Health          *Health         `yaml:"health,omitempty"`
	Limits          *ListenerLimits `yaml:"limits,omitempty"`
}

type UDP struct {
	SessionIdleTimeout   Duration `yaml:"sessionIdleTimeout"`
	MaxSessions          int      `yaml:"maxSessions"`
	MaxSessionsPerSource int      `yaml:"maxSessionsPerSource"`
	BufferSize           int      `yaml:"bufferSize"`
}

// PoolSpec describes one backend pool. Key is "<name>" for tcp/udp and
// "<name>/<route>" for tls routes. Namespaces is DiscoveryNamespaces() of the
// owning configuration ([""] = cluster-wide).
type PoolSpec struct {
	Key                      string
	ConfigName               string
	Route                    string
	BackendPortName          string
	Protocol                 Protocol
	Balancer                 Balancer
	Health                   Health
	MaxConnectionsPerBackend int
	Namespaces               []string
}
