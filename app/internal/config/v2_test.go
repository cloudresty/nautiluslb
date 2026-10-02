package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func tlsConfiguration() Configuration {
	return Configuration{
		Name: "https", Protocol: ProtocolTLS, ListenerAddress: ":443", Namespaces: []string{"apps"},
		BackendPortName: "https",
		TLS: &TLS{Routes: []Route{
			{Name: "web", Hosts: []string{"example.com", "*.example.com"}},
			{Name: "api", Hosts: []string{"api.example.com"}, BackendPortName: "api"},
		}},
	}
}

func udpConfiguration() Configuration {
	return Configuration{Name: "dns", Protocol: ProtocolUDP, ListenerAddress: ":53", Namespaces: []string{"dns"}, BackendPortName: "dns-udp"}
}

// TestValidateMatrix covers every rule of plan A.3 (valid and invalid).
func TestValidateMatrix(t *testing.T) {
	type mut func(*Config)
	on := func(c Configuration, f func(*Configuration)) mut {
		return func(x *Config) { f(&c); x.Configurations = []Configuration{c} }
	}
	tlsOn := func(f func(*Configuration)) mut { return on(tlsConfiguration(), f) }
	udpOn := func(f func(*Configuration)) mut { return on(udpConfiguration(), f) }
	tcpOn := func(f func(*Configuration)) mut { return on(validConfiguration(), f) }
	settings := func(f func(*Settings)) mut { return func(x *Config) { f(&x.Settings) } }

	tests := []struct {
		name    string
		m       mut
		wantErr string
	}{
		// rule 1
		{"r1 valid", func(*Config) {}, ""},
		{"r1 v1 file", func(x *Config) { x.APIVersion, x.Kind = "", "" }, "docs/upgrading-v2.md"},
		{"r1 wrong apiVersion", func(x *Config) { x.APIVersion = "nautiluslb.cloudresty.io/v1" }, "apiVersion must be"},
		{"r1 wrong kind", func(x *Config) { x.Kind = "Other" }, "kind must be"},
		{"r1 kind missing", func(x *Config) { x.Kind = "" }, "kind must be"},
		// rule 2
		{"r2 route valid name", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Name = "a.b-c" }), ""},
		{"r2 route bad name", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Name = "a/b" }), "tls.routes[0] (a/b): invalid name"},
		{"r2 route empty name", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Name = "" }), "'name' cannot be empty"},
		{"r2 duplicate route", tlsOn(func(c *Configuration) { c.TLS.Routes[1].Name = "web" }), "duplicate route name"},
		// rule 3 (v1 listener rules covered in config_test.go)
		{"r3 valid tcp+udp same port", func(x *Config) {
			a, b := validConfiguration(), udpConfiguration()
			a.ListenerAddress, b.ListenerAddress = ":53", ":53"
			x.Configurations = []Configuration{a, b}
		}, ""},
		{"r3 tls+tcp same port", func(x *Config) {
			a, b := tlsConfiguration(), validConfiguration()
			b.ListenerAddress = ":443"
			x.Configurations = []Configuration{a, b}
		}, "conflicts with configurations[0]"},
		// rule 4
		{"r4 valid star", tcpOn(func(c *Configuration) { c.Namespaces = []string{"*"} }), ""},
		{"r4 required", tcpOn(func(c *Configuration) { c.Namespaces = nil }), "no namespaces configured"},
		{"r4 star alone", tcpOn(func(c *Configuration) { c.Namespaces = []string{"*", "a"} }), "must be the only entry"},
		{"r4 dns label", tcpOn(func(c *Configuration) { c.Namespaces = []string{"A_b"} }), "invalid namespace"},
		// rule 5
		{"r5 routes supply port", tlsOn(func(c *Configuration) {
			c.BackendPortName = ""
			c.TLS.Routes[0].BackendPortName = "https"
		}), ""},
		{"r5 missing for a route", tlsOn(func(c *Configuration) { c.BackendPortName = "" }), "'backendPortName' cannot be empty"},
		{"r5 route bad port", tlsOn(func(c *Configuration) { c.TLS.Routes[1].BackendPortName = "A_B" }), "invalid backendPortName"},
		{"r5 tcp requires", tcpOn(func(c *Configuration) { c.BackendPortName = "" }), "'backendPortName' cannot be empty"},
		// rule 6
		{"r6 tls valid", tlsOn(func(c *Configuration) {}), ""},
		{"r6 tls default route ok", tlsOn(func(c *Configuration) { c.TLS.DefaultRoute = "api" }), ""},
		{"r6 tls no block", tlsOn(func(c *Configuration) { c.TLS = nil }), "requires a 'tls' block"},
		{"r6 tls no routes", tlsOn(func(c *Configuration) { c.TLS.Routes = nil }), "at least one route"},
		{"r6 route no hosts", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Hosts = nil }), "needs at least one host"},
		{"r6 host uppercase", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Hosts = []string{"Example.com"} }), "must be lowercase"},
		{"r6 host mid wildcard", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Hosts = []string{"a.*.com"} }), "invalid host"},
		{"r6 host bare star", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Hosts = []string{"*"} }), "invalid host"},
		{"r6 host trailing wildcard", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Hosts = []string{"example.*"} }), "invalid host"},
		{"r6 host underscore", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Hosts = []string{"a_b.com"} }), "invalid host"},
		{"r6 host duplicate across routes", tlsOn(func(c *Configuration) { c.TLS.Routes[1].Hosts = []string{"example.com"} }), "already routed by"},
		{"r6 default route unknown", tlsOn(func(c *Configuration) { c.TLS.DefaultRoute = "nope" }), "does not name a route"},
		{"r6 tls with udp block", tlsOn(func(c *Configuration) { c.UDP = &UDP{} }), "'udp' block is only valid"},
		{"r6 tls peek timeout", tlsOn(func(c *Configuration) { c.TLS.PeekTimeout = -1 }), "tls.peekTimeout"},
		{"r6 tls max hello", tlsOn(func(c *Configuration) { c.TLS.MaxClientHello = 70000 }), "tls.maxClientHello"},
		{"r6 udp valid", udpOn(func(c *Configuration) {}), ""},
		{"r6 udp with tls", udpOn(func(c *Configuration) { c.TLS = &TLS{} }), "'tls' block is only valid"},
		{"r6 udp proxy out", udpOn(func(c *Configuration) { c.ProxyProtocol.Out = "v2" }), "proxyProtocol.out is not supported"},
		{"r6 udp idleTimeout", udpOn(func(c *Configuration) { c.IdleTimeout = Duration(time.Second) }), "udp.sessionIdleTimeout"},
		{"r6 udp buffer", udpOn(func(c *Configuration) { c.UDP = &UDP{BufferSize: 10} }), "udp.bufferSize"},
		{"r6 udp sessions", udpOn(func(c *Configuration) { c.UDP = &UDP{MaxSessions: -1} }), "udp.maxSessions"},
		{"r6 tcp with tls", tcpOn(func(c *Configuration) { c.TLS = &TLS{} }), "'tls' block is only valid"},
		{"r6 tcp with udp", tcpOn(func(c *Configuration) { c.UDP = &UDP{} }), "'udp' block is only valid"},
		{"r6 bad protocol", tcpOn(func(c *Configuration) { c.Protocol = "sctp" }), "invalid protocol"},
		// rule 7
		{"r7 valid", tcpOn(func(c *Configuration) {
			c.Balancer = Balancer{Algorithm: "random_two_choices", SlowStart: Duration(time.Second)}
		}), ""},
		{"r7 bad algorithm", tcpOn(func(c *Configuration) { c.Balancer.Algorithm = "magic" }), "invalid balancer.algorithm"},
		{"r7 negative slowStart", tcpOn(func(c *Configuration) { c.Balancer.SlowStart = -1 }), "balancer.slowStart"},
		{"r7 route bad algorithm", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Balancer = &Balancer{Algorithm: "x"} }), "tls.routes[0] (web): invalid balancer.algorithm"},
		// rule 8
		{"r8 http valid", tcpOn(func(c *Configuration) {
			c.Health = Health{Type: "http", HTTP: HTTPProbe{Path: "/h", ExpectStatus: []int{200}}}
		}), ""},
		{"r8 none valid", tcpOn(func(c *Configuration) { c.Health.Type = "none" }), ""},
		{"r8 bad type", tcpOn(func(c *Configuration) { c.Health.Type = "icmp" }), "invalid type"},
		{"r8 interval <= timeout", tcpOn(func(c *Configuration) {
			c.Health.Interval, c.Health.Timeout = Duration(time.Second), Duration(2*time.Second)
		}), "must be greater than timeout"},
		{"r8 negative timeout", tcpOn(func(c *Configuration) { c.Health.Timeout = -1 }), "invalid timeout"},
		{"r8 negative rise", tcpOn(func(c *Configuration) { c.Health.Rise = -1 }), "invalid rise"},
		{"r8 negative fall", tcpOn(func(c *Configuration) { c.Health.Fall = -1 }), "invalid fall"},
		{"r8 jitter 1", tcpOn(func(c *Configuration) { c.Health.Jitter = 1 }), "invalid jitter"},
		{"r8 jitter negative", tcpOn(func(c *Configuration) { c.Health.Jitter = -0.1 }), "invalid jitter"},
		{"r8 http path", tcpOn(func(c *Configuration) { c.Health = Health{Type: "http", HTTP: HTTPProbe{Path: "h"}} }), "http.path"},
		{"r8 http status", tcpOn(func(c *Configuration) {
			c.Health = Health{Type: "http", HTTP: HTTPProbe{Path: "/", ExpectStatus: []int{99}}}
		}), "http.expectStatus"},
		{"r8 http status high", tcpOn(func(c *Configuration) {
			c.Health = Health{Type: "http", HTTP: HTTPProbe{Path: "/", ExpectStatus: []int{600}}}
		}), "http.expectStatus"},
		{"r8 port range", tcpOn(func(c *Configuration) { c.Health.Port = 70000 }), "invalid port"},
		{"r8 udp http without port", udpOn(func(c *Configuration) { c.Health.Type = "http" }), "needs an explicit 'port'"},
		{"r8 udp http with port", udpOn(func(c *Configuration) { c.Health = Health{Type: "http", Port: 8080} }), ""},
		{"r8 route health bad", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Health = &Health{Type: "http", HTTP: HTTPProbe{Path: "x"}} }), "tls.routes[0] (web): health: invalid http.path"},
		// rule 9
		{"r9 limits valid", tcpOn(func(c *Configuration) { c.Limits = ListenerLimits{1, 2, 3} }), ""},
		{"r9 negative limit", tcpOn(func(c *Configuration) { c.Limits.MaxConnectionsPerSource = -1 }), "limits.maxConnectionsPerSource"},
		{"r9 route only per backend", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Limits = &ListenerLimits{MaxConnections: 1} }), "only maxConnectionsPerBackend"},
		{"r9 route limit negative", tlsOn(func(c *Configuration) { c.TLS.Routes[0].Limits = &ListenerLimits{MaxConnectionsPerBackend: -1} }), "maxConnectionsPerBackend"},
		{"r9 dial timeout 10s", tcpOn(func(c *Configuration) { c.DialTimeout = Duration(10 * time.Second) }), ""},
		{"r9 dial timeout over cap is capped not rejected", tcpOn(func(c *Configuration) { c.DialTimeout = Duration(time.Minute) }), ""},
		{"r9 dial timeout negative", tcpOn(func(c *Configuration) { c.DialTimeout = -1 }), "invalid dialTimeout"},
		{"r9 idle timeout negative", tcpOn(func(c *Configuration) { c.IdleTimeout = -1 }), "invalid idleTimeout"},
		{"r9 idle timeout valid", tcpOn(func(c *Configuration) { c.IdleTimeout = Duration(time.Minute) }), ""},
		// rule 10
		{"r10 cidrs valid", tcpOn(func(c *Configuration) {
			c.Access = Access{Allow: []string{"10.0.0.0/8", "::1/128"}, Deny: []string{"192.0.2.1/32"}}
		}), ""},
		{"r10 bad allow", tcpOn(func(c *Configuration) { c.Access.Allow = []string{"10.0.0.1"} }), "invalid access.allow entry"},
		{"r10 bad deny", tcpOn(func(c *Configuration) { c.Access.Deny = []string{"x"} }), "invalid access.deny entry"},
		{"r10 bad trusted", tcpOn(func(c *Configuration) { c.ProxyProtocol.In.TrustedCIDRs = []string{"x"} }), "trustedCIDRs"},
		{"r10 out v1", tcpOn(func(c *Configuration) { c.ProxyProtocol.Out = "v1" }), ""},
		{"r10 out bad", tcpOn(func(c *Configuration) { c.ProxyProtocol.Out = "v3" }), "invalid proxyProtocol.out"},
		{"r10 required without trusted", tcpOn(func(c *Configuration) { c.ProxyProtocol.In.Required = true }), "needs proxyProtocol.in.trustedCIDRs"},
		{"r10 required with trusted", tcpOn(func(c *Configuration) {
			c.ProxyProtocol.In = ProxyProtocolIn{Required: true, TrustedCIDRs: []string{"10.0.0.0/8"}}
		}), ""},
		// rule 11
		{"r11 log level case", settings(func(s *Settings) { s.LogLevel = "WARN" }), ""},
		{"r11 bad log level", settings(func(s *Settings) { s.LogLevel = "loud" }), "invalid logLevel"},
		{"r11 admin disabled", settings(func(s *Settings) { s.Admin.Address = "" }), ""},
		{"r11 admin bad", settings(func(s *Settings) { s.Admin.Address = "9090" }), "invalid admin.address"},
		{"r11 admin bad port", settings(func(s *Settings) { s.Admin.Address = ":99999" }), "invalid admin.address"},
		{"r11 accessLog file", settings(func(s *Settings) { s.AccessLog.Output = "/var/log/a.log" }), ""},
		{"r11 accessLog stderr", settings(func(s *Settings) { s.AccessLog.Output = "stderr" }), ""},
		{"r11 accessLog relative", settings(func(s *Settings) { s.AccessLog.Output = "a.log" }), "invalid accessLog.output"},
		{"r11 accessLog buffer", settings(func(s *Settings) { s.AccessLog.BufferSize = -1 }), "accessLog.bufferSize"},
		{"r11 drain timeout", settings(func(s *Settings) { s.Drain.Timeout = -1 }), "drain.timeout"},
		{"r11 drain delay", settings(func(s *Settings) { s.Drain.ReadinessDelay = -1 }), "drain.readinessDelay"},
		{"r11 address family", settings(func(s *Settings) { s.Discovery.Nodes.AddressFamily = "ipx" }), "addressFamily"},
		{"r11 address family ok", settings(func(s *Settings) { s.Discovery.Nodes.AddressFamily = "prefer-ipv6" }), ""},
		{"r11 qps negative", settings(func(s *Settings) { s.Kubernetes.QPS = -1 }), "kubernetes.qps"},
		{"r11 burst < qps", settings(func(s *Settings) { s.Kubernetes.QPS, s.Kubernetes.Burst = 50, 10 }), "kubernetes.burst"},
		{"r11 selector key", settings(func(s *Settings) { s.Discovery.Nodes.Selector = map[string]string{"bad key": "v"} }), "selector key"},
		{"r11 selector ok", settings(func(s *Settings) { s.Discovery.Nodes.Selector = map[string]string{"pool": "edge"} }), ""},
		{"r11 max connections", settings(func(s *Settings) { s.Limits.MaxConnections = -1 }), "limits.maxConnections"},
		{"r11 resync", settings(func(s *Settings) { s.Discovery.ResyncPeriod = -1 }), "resyncPeriod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.m(c)
			expectErr(t, c.Validate(), tt.wantErr)
		})
	}
}

func TestDeprecatedAliases(t *testing.T) {
	t.Run("alias alone maps and warns", func(t *testing.T) {
		c := validConfig()
		c.Settings.KubeconfigPath = "/k"
		c.Configurations[0].RequestTimeout = 7
		c.Configurations[0].Namespace = "extra"
		c.ApplyDefaults()
		expectErr(t, c.Validate(), "")
		if c.Settings.Kubernetes.Kubeconfig != "/k" {
			t.Errorf("kubeconfig = %q", c.Settings.Kubernetes.Kubeconfig)
		}
		if got := c.Configurations[0].DialTimeout.Std(); got != 7*time.Second {
			t.Errorf("dialTimeout = %v", got)
		}
		if got := c.Configurations[0].DiscoveryNamespaces(); !reflect.DeepEqual(got, []string{"default", "extra"}) {
			t.Errorf("namespaces = %v", got)
		}
		d := strings.Join(c.Deprecations(), "\n")
		for _, want := range []string{"kubeconfigPath", "requestTimeout", "namespace is deprecated"} {
			if !strings.Contains(d, want) {
				t.Errorf("deprecations missing %q:\n%s", want, d)
			}
		}
	})
	t.Run("conflicting kubeconfig", func(t *testing.T) {
		c := validConfig()
		c.Settings.KubeconfigPath = "/old"
		c.Settings.Kubernetes.Kubeconfig = "/new"
		c.ApplyDefaults()
		expectErr(t, c.Validate(), "kubeconfigPath (deprecated)")
	})
	t.Run("matching kubeconfig ok", func(t *testing.T) {
		c := validConfig()
		c.Settings.KubeconfigPath = "/same"
		c.Settings.Kubernetes.Kubeconfig = "/same"
		c.ApplyDefaults()
		expectErr(t, c.Validate(), "")
	})
	t.Run("conflicting dialTimeout", func(t *testing.T) {
		c := validConfig()
		c.Configurations[0].RequestTimeout = 7
		c.Configurations[0].DialTimeout = Duration(3 * time.Second)
		c.ApplyDefaults()
		expectErr(t, c.Validate(), "requestTimeout (deprecated) 7 conflicts with dialTimeout")
	})
	t.Run("equal dialTimeout ok", func(t *testing.T) {
		c := validConfig()
		c.Configurations[0].RequestTimeout = 3
		c.Configurations[0].DialTimeout = Duration(3 * time.Second)
		c.ApplyDefaults()
		expectErr(t, c.Validate(), "")
	})
	t.Run("parsed aliases and bare ints", func(t *testing.T) {
		cfg, err := Parse("a.yaml", []byte(header+`settings:
  kubeconfigPath: /k
  drain: {timeout: 20}
configurations:
  - name: a
    listenerAddress: ":80"
    backendPortName: http
    namespace: ns
    requestTimeout: 4
    health: {interval: 15, timeout: 0}
`))
		if err != nil {
			t.Fatal(err)
		}
		cfg.ApplyDefaults()
		expectErr(t, cfg.Validate(), "")
		d := strings.Join(cfg.Deprecations(), "\n")
		for _, want := range []string{"settings.drain.timeout: bare integer", "configurations[0].health.interval: bare integer", "requestTimeout"} {
			if !strings.Contains(d, want) {
				t.Errorf("deprecations missing %q:\n%s", want, d)
			}
		}
		if strings.Contains(d, "health.timeout") {
			t.Errorf("bare 0 must not warn:\n%s", d)
		}
		if cfg.Settings.Drain.Timeout.Std() != 20*time.Second {
			t.Errorf("bare int not seconds")
		}
	})
}

func TestApplyEnvTable(t *testing.T) {
	env := func(kv ...string) func(string) (string, bool) {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	tests := []struct {
		name    string
		env     func(string) (string, bool)
		check   func(*Config) bool
		wantErr string
	}{
		{"none", env(), func(c *Config) bool {
			return c.Settings.LogLevel == "info" && c.Settings.Admin.Address == "127.0.0.1:9090"
		}, ""},
		{"log level", env("NLB_LOG_LEVEL", "error"), func(c *Config) bool { return c.Settings.LogLevel == "error" }, ""},
		{"kubeconfig", env("NLB_KUBECONFIG", "/x"), func(c *Config) bool {
			return c.Settings.Kubernetes.Kubeconfig == "/x" && c.Settings.KubeconfigPath == ""
		}, ""},
		{"admin address", env("NLB_ADMIN_ADDRESS", ":9100"), func(c *Config) bool { return c.Settings.Admin.Address == ":9100" }, ""},
		{"admin disabled", env("NLB_ADMIN_ADDRESS", ""), func(c *Config) bool { return c.Settings.Admin.Address == "" }, ""},
		{"pprof", env("NLB_PPROF", "true"), func(c *Config) bool { return c.Settings.Admin.Pprof }, ""},
		{"pprof 1", env("NLB_PPROF", "1"), func(c *Config) bool { return c.Settings.Admin.Pprof }, ""},
		{"pprof bad", env("NLB_PPROF", "maybe"), nil, "NLB_PPROF"},
		{"access log off", env("NLB_ACCESS_LOG_ENABLED", "false"), func(c *Config) bool { return !c.Settings.AccessLog.Enabled }, ""},
		{"access log bad", env("NLB_ACCESS_LOG_ENABLED", "x"), nil, "NLB_ACCESS_LOG_ENABLED"},
		{"max connections", env("NLB_MAX_CONNECTIONS", "500"), func(c *Config) bool { return c.Settings.Limits.MaxConnections == 500 }, ""},
		{"max connections bad", env("NLB_MAX_CONNECTIONS", "-1"), nil, "NLB_MAX_CONNECTIONS"},
		{"max connections nan", env("NLB_MAX_CONNECTIONS", "x"), nil, "NLB_MAX_CONNECTIONS"},
		{"drain", env("NLB_DRAIN_TIMEOUT", "1m"), func(c *Config) bool { return c.Settings.Drain.Timeout.Std() == time.Minute }, ""},
		{"drain seconds", env("NLB_DRAIN_TIMEOUT", "45"), func(c *Config) bool { return c.Settings.Drain.Timeout.Std() == 45*time.Second }, ""},
		{"drain bad", env("NLB_DRAIN_TIMEOUT", "soon"), nil, "NLB_DRAIN_TIMEOUT"},
		{"several bad", env("NLB_PPROF", "x", "NLB_DRAIN_TIMEOUT", "y"), nil, "NLB_DRAIN_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			c.ApplyDefaults()
			err := c.ApplyEnv(tt.env)
			expectErr(t, err, tt.wantErr)
			if tt.wantErr == "" && !tt.check(c) {
				t.Errorf("check failed: %+v", c.Settings)
			}
		})
	}
	// both bad variables are reported together
	c := validConfig()
	err := c.ApplyEnv(env("NLB_PPROF", "x", "NLB_DRAIN_TIMEOUT", "y"))
	expectErr(t, err, "NLB_PPROF")
	expectErr(t, err, "NLB_DRAIN_TIMEOUT")
	// env value flows into validation
	c = validConfig()
	c.ApplyDefaults()
	_ = c.ApplyEnv(env("NLB_LOG_LEVEL", "loud"))
	expectErr(t, c.Validate(), "invalid logLevel")
}

func TestDefaults(t *testing.T) {
	c := validConfig(validConfiguration(), udpConfiguration(), tlsConfiguration())
	c.Configurations[1].ListenerAddress = ":5353"
	c.ApplyDefaults()
	c.ApplyDefaults() // idempotent

	s := c.Settings
	if s.LogLevel != "info" || s.Kubernetes.QPS != 20 || s.Kubernetes.Burst != 40 ||
		s.Discovery.ResyncPeriod.Std() != 5*time.Minute || s.Discovery.Debounce.Std() != 200*time.Millisecond ||
		!s.Discovery.Nodes.ReadyOnly || !s.Discovery.Nodes.SkipUnschedulable || s.Discovery.Nodes.SkipControlPlane ||
		s.Discovery.Nodes.AddressFamily != "ipv4" || s.Admin.Address != "127.0.0.1:9090" || s.Admin.Pprof ||
		!s.Admin.Metrics.PerBackend || !s.AccessLog.Enabled || s.AccessLog.Output != "stdout" ||
		s.AccessLog.BufferSize != 4096 || s.Limits.MaxConnections != 0 ||
		s.Drain.ReadinessDelay.Std() != 3*time.Second || s.Drain.Timeout.Std() != 30*time.Second || s.Reload.WatchFile {
		t.Errorf("settings defaults wrong: %+v", s)
	}

	tcp := c.Configurations[0]
	if tcp.Protocol != ProtocolTCP || tcp.Balancer.Algorithm != "round_robin" || tcp.DialTimeout.Std() != 5*time.Second ||
		tcp.Health.Type != "tcp" || tcp.Health.Interval.Std() != 10*time.Second || tcp.Health.Timeout.Std() != 2*time.Second ||
		tcp.Health.Rise != 1 || tcp.Health.Fall != 3 || tcp.Health.Jitter != 0.2 || tcp.Health.EjectionHold.Std() != 10*time.Second ||
		tcp.UDP != nil || tcp.TLS != nil {
		t.Errorf("tcp defaults wrong: %+v", tcp)
	}
	udp := c.Configurations[1]
	if udp.Health.Type != "none" || udp.UDP == nil || udp.UDP.SessionIdleTimeout.Std() != time.Minute || udp.UDP.BufferSize != 65535 {
		t.Errorf("udp defaults wrong: %+v %+v", udp.Health, udp.UDP)
	}
	tl := c.Configurations[2]
	if tl.TLS.PeekTimeout.Std() != 5*time.Second || tl.TLS.MaxClientHello != 16384 {
		t.Errorf("tls defaults wrong: %+v", tl.TLS)
	}

	// hand-built config: an explicit false set after ApplyDefaults survives a re-run
	c.Settings.Discovery.Nodes.ReadyOnly = false
	c.ApplyDefaults()
	if c.Settings.Discovery.Nodes.ReadyOnly {
		t.Error("explicit false overwritten by second ApplyDefaults")
	}
	// ApplyDefaults must not overwrite a hand-set admin address
	h := validConfig()
	h.Settings.Admin.Address = ":1234"
	h.ApplyDefaults()
	if h.Settings.Admin.Address != ":1234" {
		t.Errorf("admin address overwritten: %q", h.Settings.Admin.Address)
	}
}

func TestParseExplicitFalseAndZero(t *testing.T) {
	cfg, err := Parse("e.yaml", []byte(header+`settings:
  discovery: {nodes: {readyOnly: false, skipUnschedulable: false}}
  accessLog: {enabled: false}
  admin: {address: "", metrics: {perBackend: false}}
configurations:
  - name: a
    listenerAddress: ":80"
    backendPortName: http
    namespaces: [d]
    health: {jitter: 0}
  - name: b
    listenerAddress: ":81"
    backendPortName: http
    namespaces: [d]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	s := cfg.Settings
	if s.Discovery.Nodes.ReadyOnly || s.Discovery.Nodes.SkipUnschedulable || s.AccessLog.Enabled || s.Admin.Metrics.PerBackend || s.Admin.Address != "" {
		t.Errorf("explicit false not honoured: %+v", s)
	}
	if cfg.Configurations[0].Health.Jitter != 0 || cfg.Configurations[1].Health.Jitter != 0.2 {
		t.Errorf("jitter: explicit 0 = %v, default = %v", cfg.Configurations[0].Health.Jitter, cfg.Configurations[1].Health.Jitter)
	}
	expectErr(t, cfg.Validate(), "")
}

func TestPools(t *testing.T) {
	c := validConfig(tlsConfiguration(), validConfiguration(), udpConfiguration())
	c.Configurations[1].ListenerAddress = ":8080"
	c.Configurations[2].ListenerAddress = ":8080"
	c.Configurations[0].Balancer = Balancer{Algorithm: "round_robin", SlowStart: Duration(30 * time.Second)}
	c.Configurations[0].Limits.MaxConnectionsPerBackend = 100
	c.Configurations[0].TLS.Routes[0].Balancer = &Balancer{Algorithm: "least_conn"}
	c.Configurations[0].TLS.Routes[0].Health = &Health{Type: "http", Interval: Duration(20 * time.Second), HTTP: HTTPProbe{Path: "/r"}}
	c.Configurations[0].TLS.Routes[0].Limits = &ListenerLimits{MaxConnectionsPerBackend: 7}
	c.ApplyDefaults()
	expectErr(t, c.Validate(), "")

	tlsPools := c.Configurations[0].Pools()
	if len(tlsPools) != 2 {
		t.Fatalf("tls pools = %d", len(tlsPools))
	}
	web, api := tlsPools[0], tlsPools[1]
	if web.Key != "https/web" || web.ConfigName != "https" || web.Route != "web" || web.BackendPortName != "https" || web.Protocol != ProtocolTLS {
		t.Errorf("web identity: %+v", web)
	}
	if web.Balancer.Algorithm != "least_conn" || web.Balancer.SlowStart.Std() != 30*time.Second {
		t.Errorf("web balancer should override algorithm and inherit slowStart: %+v", web.Balancer)
	}
	if web.Health.Type != "http" || web.Health.Interval.Std() != 20*time.Second || web.Health.Timeout.Std() != 2*time.Second ||
		web.Health.Fall != 3 || web.Health.HTTP.Path != "/r" || !reflect.DeepEqual(web.Health.HTTP.ExpectStatus, []int{200, 204}) {
		t.Errorf("web health merge: %+v", web.Health)
	}
	if web.MaxConnectionsPerBackend != 7 || api.MaxConnectionsPerBackend != 100 {
		t.Errorf("per-backend cap: web %d api %d", web.MaxConnectionsPerBackend, api.MaxConnectionsPerBackend)
	}
	if api.Key != "https/api" || api.BackendPortName != "api" || api.Balancer.Algorithm != "round_robin" || api.Health.Type != "tcp" {
		t.Errorf("api: %+v", api)
	}
	if !reflect.DeepEqual(web.Namespaces, []string{"apps"}) {
		t.Errorf("namespaces: %v", web.Namespaces)
	}

	tcp := c.Configurations[1].Pools()
	if len(tcp) != 1 || tcp[0].Key != "web" || tcp[0].Route != "" || tcp[0].Protocol != ProtocolTCP {
		t.Errorf("tcp pool: %+v", tcp)
	}
	udp := c.Configurations[2].Pools()
	if len(udp) != 1 || udp[0].Key != "dns" || udp[0].Health.Type != "none" {
		t.Errorf("udp pool: %+v", udp)
	}
}

func TestUDPMaxSessionsDefault(t *testing.T) {
	t.Run("no udp block", func(t *testing.T) {
		c := udpConfiguration()
		c.UDP = nil
		cfg := &Config{Configurations: []Configuration{c}}
		cfg.ApplyDefaults()
		if got := cfg.Configurations[0].UDP.MaxSessions; got != DefaultUDPMaxSessions {
			t.Fatalf("maxSessions = %d", got)
		}
	})
	yml := func(udp string) *Config {
		cfg, err := Parse("t", []byte("configurations:\n  - name: dns\n    protocol: udp\n    listenerAddress: \":53\"\n    udp: "+udp+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if got := yml("{}").Configurations[0].UDP.MaxSessions; got != DefaultUDPMaxSessions {
		t.Fatalf("absent key = %d", got)
	}
	cfg := yml("{maxSessions: 0}")
	if got := cfg.Configurations[0].UDP.MaxSessions; got != 0 {
		t.Fatalf("explicit 0 = %d", got)
	}
	found := false
	for _, d := range cfg.Deprecations() {
		found = found || strings.Contains(d, "udp.maxSessions")
	}
	if !found {
		t.Fatal("no warning for explicit 0")
	}
}
