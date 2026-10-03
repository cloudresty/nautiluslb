package config

import (
	"fmt"
	"time"
)

const (
	HealthTCP  = "tcp"
	HealthHTTP = "http"
	HealthNone = "none"

	defaultDialTimeout = 5 * time.Second
)

// seedBools sets the true-valued defaults. Parse calls it before decoding so an
// explicit false in the file survives; ApplyDefaults calls it once for configs
// that were never parsed.
func (c *Config) seedBools() {
	c.Settings.Discovery.Nodes.ReadyOnly = true
	c.Settings.Discovery.Nodes.SkipUnschedulable = true
	c.Settings.AccessLog.Enabled = true
	c.Settings.Admin.Metrics.PerBackend = true
	// An explicit admin.address "" disables the admin server, so its default
	// is seeded here (not in ApplyDefaults) and only over an empty value.
	if c.Settings.Admin.Address == "" {
		c.Settings.Admin.Address = "127.0.0.1:9090"
	}
	c.seeded = true
}

// ApplyDefaults fills every unset field with its default and maps the
// deprecated v0.x aliases onto their replacements. It is idempotent.
func (c *Config) ApplyDefaults() {
	if !c.seeded {
		c.seedBools()
	}

	s := &c.Settings
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	if s.Kubernetes.Kubeconfig == "" {
		s.Kubernetes.Kubeconfig = s.KubeconfigPath
	}
	if s.Kubernetes.QPS == 0 {
		s.Kubernetes.QPS = 20
	}
	if s.Kubernetes.Burst == 0 {
		s.Kubernetes.Burst = 40
	}
	if s.Discovery.ResyncPeriod == 0 {
		s.Discovery.ResyncPeriod = Duration(5 * time.Minute)
	}
	if s.Discovery.Debounce == 0 {
		s.Discovery.Debounce = Duration(200 * time.Millisecond)
	}
	if s.Discovery.Nodes.AddressFamily == "" {
		s.Discovery.Nodes.AddressFamily = "ipv4"
	}
	if s.AccessLog.Output == "" {
		s.AccessLog.Output = "stdout"
	}
	if s.AccessLog.BufferSize == 0 {
		s.AccessLog.BufferSize = 4096
	}
	if s.Drain.ReadinessDelay == 0 {
		s.Drain.ReadinessDelay = Duration(3 * time.Second)
	}
	if s.Drain.Timeout == 0 {
		s.Drain.Timeout = Duration(30 * time.Second)
	}

	for i := range c.Configurations {
		c.Configurations[i].applyDefaults()
	}
}

func (c *Configuration) applyDefaults() {
	// Deprecated aliases: map when the new field is unset; a conflict
	// (both set, different) is left in place for Validate to report.
	if c.RequestTimeout > 0 && c.DialTimeout == 0 {
		c.DialTimeout = Duration(time.Duration(c.RequestTimeout) * time.Second)
	}

	if c.Protocol == "" {
		c.Protocol = ProtocolTCP
	}
	if c.Balancer.Algorithm == "" {
		c.Balancer.Algorithm = "round_robin"
	}

	h := &c.Health
	if h.Type == "" {
		// There is no generic UDP probe: UDP pools rely on passive ejection.
		if c.Protocol == ProtocolUDP {
			h.Type = HealthNone
		} else {
			h.Type = HealthTCP
		}
	}
	if h.Interval == 0 {
		h.Interval = Duration(10 * time.Second)
	}
	if h.Timeout == 0 {
		h.Timeout = Duration(2 * time.Second)
	}
	if h.Rise == 0 {
		h.Rise = 1
	}
	if h.Fall == 0 {
		h.Fall = 3
	}
	if !c.seeded {
		if h.Jitter == 0 {
			h.Jitter = 0.2
		}
		c.seeded = true
	}
	if h.EjectionHold == 0 {
		h.EjectionHold = Duration(10 * time.Second)
	}
	*h = h.withHTTPDefaults()

	if c.DialTimeout == 0 {
		c.DialTimeout = Duration(defaultDialTimeout)
	}

	if c.TLS != nil {
		if c.TLS.PeekTimeout == 0 {
			c.TLS.PeekTimeout = Duration(5 * time.Second)
		}
		if c.TLS.MaxClientHello == 0 {
			c.TLS.MaxClientHello = 16384
		}
	}

	if c.Protocol == ProtocolUDP && c.UDP == nil {
		c.UDP = &UDP{MaxSessions: DefaultUDPMaxSessions}
	}
	if c.UDP != nil {
		if c.UDP.SessionIdleTimeout == 0 {
			c.UDP.SessionIdleTimeout = Duration(60 * time.Second)
		}
		if c.UDP.BufferSize == 0 {
			c.UDP.BufferSize = 65535
		}
	}
}

// Deprecations lists the deprecated constructs in the configuration: v0.x key
// aliases still in use and bare-integer durations. Safe to call at any stage.
func (c *Config) Deprecations() []string {
	out := append([]string(nil), c.parseWarning...)
	if c.Settings.KubeconfigPath != "" {
		out = append(out, "settings.kubeconfigPath is deprecated: use settings.kubernetes.kubeconfig")
	}
	for i := range c.Configurations {
		cf := &c.Configurations[i]
		p := fmt.Sprintf("configurations[%d] (%s)", i, cf.Name)
		if cf.RequestTimeout != 0 {
			out = append(out, p+": requestTimeout (integer seconds) is deprecated: use dialTimeout, e.g. \"5s\"")
		}
		if cf.Namespace != "" {
			out = append(out, p+": namespace is deprecated: use namespaces: [\""+cf.Namespace+"\"]")
		}
		if d := cf.DialTimeout.Std(); d > MaxDialTimeout {
			out = append(out, fmt.Sprintf("%s: dialTimeout %s exceeds the %s cap and will be capped", p, d, MaxDialTimeout))
		}
		if cf.Protocol == ProtocolUDP && cf.UDP != nil && cf.UDP.MaxSessions == 0 {
			out = append(out, p+": udp.maxSessions: 0 means unlimited sessions; memory grows as sessions x (bufferSize + ~10KB), the default is 4096")
		}
	}
	return out
}
