package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const upgradeHint = "this looks like a v0.x config file (no apiVersion): set apiVersion: " + APIVersion +
	" and kind: " + Kind + "; see docs/upgrading-v1.md"

var (
	logLevels       = []string{"debug", "info", "information", "warn", "warning", "error"}
	algorithms      = []string{"round_robin", "least_conn", "source_ip_hash", "random_two_choices"}
	healthTypes     = []string{HealthTCP, HealthHTTP, HealthNone}
	addressFamilies = []string{"ipv4", "ipv6", "prefer-ipv4", "prefer-ipv6"}
)

// effective returns a copy with defaults applied, leaving c untouched, so
// Validate also works on a config that never went through ApplyDefaults.
func (c *Config) effective() *Config {
	cp := *c
	cp.Configurations = make([]Configuration, len(c.Configurations))
	copy(cp.Configurations, c.Configurations)
	for i := range cp.Configurations {
		if t := cp.Configurations[i].TLS; t != nil {
			tt := *t
			cp.Configurations[i].TLS = &tt
		}
		if u := cp.Configurations[i].UDP; u != nil {
			uu := *u
			cp.Configurations[i].UDP = &uu
		}
	}
	cp.ApplyDefaults()
	return &cp
}

// family groups protocols that compete for the same port space.
func (p Protocol) family() string {
	if p == ProtocolUDP {
		return "udp"
	}
	return "tcp"
}

// Validate checks the whole configuration and reports every problem at once.
func (c *Config) Validate() error {
	v := c.effective()
	var errs []error

	if v.APIVersion == "" && v.Kind == "" {
		errs = append(errs, errors.New(upgradeHint))
	} else {
		if v.APIVersion != APIVersion {
			errs = append(errs, fmt.Errorf("apiVersion must be %q, got %q (see docs/upgrading-v1.md)", APIVersion, v.APIVersion))
		}
		if v.Kind != Kind {
			errs = append(errs, fmt.Errorf("kind must be %q, got %q", Kind, v.Kind))
		}
	}

	errs = append(errs, v.settingsProblems()...)

	if len(v.Configurations) == 0 {
		errs = append(errs, errors.New("no configurations defined: 'configurations' must contain at least one entry"))
		return errors.Join(errs...)
	}

	prefix := func(i int) string {
		return fmt.Sprintf("configurations[%d] (%s)", i, v.Configurations[i].Name)
	}

	names := make(map[string]int)
	type listener struct {
		host string
		port int
	}
	listeners := make(map[int]listener)

	for i := range v.Configurations {
		bc := &v.Configurations[i]

		for _, err := range bc.problems() {
			errs = append(errs, fmt.Errorf("%s: %w", prefix(i), err))
		}

		if bc.Name != "" {
			if first, ok := names[bc.Name]; ok {
				errs = append(errs, fmt.Errorf("%s: duplicate name, already used by configurations[%d]", prefix(i), first))
			} else {
				names[bc.Name] = i
			}
		}

		host, port, err := parseListenerAddress(bc.ListenerAddress)
		if err != nil {
			continue
		}

		for j := 0; j < i; j++ {
			other, ok := listeners[j]
			if !ok || v.Configurations[j].Protocol.family() != bc.Protocol.family() {
				continue
			}
			if listenersConflict(host, port, other.host, other.port) {
				errs = append(errs, fmt.Errorf("%s: listenerAddress %q conflicts with configurations[%d] (%s) listenerAddress %q",
					prefix(i), bc.ListenerAddress, j, v.Configurations[j].Name, v.Configurations[j].ListenerAddress))
				break
			}
		}
		listeners[i] = listener{host: host, port: port}
	}

	return errors.Join(errs...)
}

func (c *Config) settingsProblems() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("settings: "+format, a...)) }
	s := &c.Settings

	if !slices.Contains(logLevels, strings.ToLower(s.LogLevel)) {
		add("invalid logLevel %q: must be one of debug, info, warn, error", s.LogLevel)
	}
	if s.KubeconfigPath != "" && s.Kubernetes.Kubeconfig != s.KubeconfigPath {
		add("kubeconfigPath (deprecated) %q conflicts with kubernetes.kubeconfig %q: remove kubeconfigPath", s.KubeconfigPath, s.Kubernetes.Kubeconfig)
	}
	if !(s.Kubernetes.QPS > 0) {
		add("invalid kubernetes.qps %v: must be > 0", s.Kubernetes.QPS)
	}
	if float64(s.Kubernetes.Burst) < float64(s.Kubernetes.QPS) {
		add("invalid kubernetes.burst %d: must be >= qps (%v)", s.Kubernetes.Burst, s.Kubernetes.QPS)
	}
	if s.Discovery.ResyncPeriod.Std() <= 0 {
		add("invalid discovery.resyncPeriod %s: must be > 0", s.Discovery.ResyncPeriod)
	}
	if s.Discovery.Debounce.Std() < 0 {
		add("invalid discovery.debounce %s: must be >= 0", s.Discovery.Debounce)
	}
	if !slices.Contains(addressFamilies, s.Discovery.Nodes.AddressFamily) {
		add("invalid discovery.nodes.addressFamily %q: must be one of %s", s.Discovery.Nodes.AddressFamily, strings.Join(addressFamilies, ", "))
	}
	for k, val := range s.Discovery.Nodes.Selector {
		if msgs := validation.IsQualifiedName(k); len(msgs) > 0 {
			add("invalid discovery.nodes.selector key %q: %s", k, strings.Join(msgs, "; "))
		}
		if msgs := validation.IsValidLabelValue(val); len(msgs) > 0 {
			add("invalid discovery.nodes.selector value %q for %q: %s", val, k, strings.Join(msgs, "; "))
		}
	}
	if a := s.Admin.Address; a != "" {
		if _, port, err := net.SplitHostPort(a); err != nil {
			add("invalid admin.address %q: %v (use \"host:port\", or \"\" to disable)", a, err)
		} else if n, err := strconv.ParseUint(port, 10, 32); err != nil || n > 65535 {
			add("invalid admin.address %q: port %q must be a number between 0 and 65535", a, port)
		}
	}
	if o := s.AccessLog.Output; o != "stdout" && o != "stderr" && !filepath.IsAbs(o) {
		add("invalid accessLog.output %q: must be stdout, stderr or an absolute file path", o)
	}
	if s.AccessLog.BufferSize <= 0 {
		add("invalid accessLog.bufferSize %d: must be > 0", s.AccessLog.BufferSize)
	}
	if s.Limits.MaxConnections < 0 {
		add("invalid limits.maxConnections %d: must be >= 0", s.Limits.MaxConnections)
	}
	if s.Drain.ReadinessDelay.Std() < 0 {
		add("invalid drain.readinessDelay %s: must be >= 0", s.Drain.ReadinessDelay)
	}
	if s.Drain.Timeout.Std() <= 0 {
		add("invalid drain.timeout %s: must be > 0", s.Drain.Timeout)
	}
	return errs
}

// problems collects every validation failure of a single (defaulted) configuration.
func (bc *Configuration) problems() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// Rule 2: name
	if bc.Name == "" {
		add("'name' cannot be empty")
	} else if !namePattern.MatchString(bc.Name) {
		add("invalid name %q: must match %s (it is referenced in a comma-separated annotation)", bc.Name, namePattern)
	}

	// Rule 3: listener
	if _, _, err := parseListenerAddress(bc.ListenerAddress); err != nil {
		errs = append(errs, err)
	}

	switch bc.Protocol {
	case ProtocolTCP, ProtocolUDP, ProtocolTLS:
	default:
		add("invalid protocol %q: must be tcp, udp or tls", bc.Protocol)
	}

	// Rule 4: namespaces
	errs = append(errs, bc.namespaceProblems()...)

	// Rule 6: protocol-specific blocks
	routesAllHavePort := false
	switch bc.Protocol {
	case ProtocolTCP:
		if bc.TLS != nil {
			add("'tls' block is only valid with protocol: tls")
		}
		if bc.UDP != nil {
			add("'udp' block is only valid with protocol: udp")
		}
	case ProtocolUDP:
		if bc.TLS != nil {
			add("'tls' block is only valid with protocol: tls")
		}
		if bc.ProxyProtocol.Out != "" {
			add("proxyProtocol.out is not supported with protocol: udp")
		}
		if bc.IdleTimeout != 0 {
			add("idleTimeout is not supported with protocol: udp: use udp.sessionIdleTimeout")
		}
		if bc.UDP != nil {
			errs = append(errs, bc.UDP.problems()...)
		}
	case ProtocolTLS:
		if bc.UDP != nil {
			add("'udp' block is only valid with protocol: udp")
		}
		if bc.TLS == nil {
			add("protocol: tls requires a 'tls' block with at least one route")
		} else {
			var rp bool
			errs = append(errs, bc.tlsProblems(&rp)...)
			routesAllHavePort = rp
		}
	}

	// Rule 5: backendPortName
	if bc.BackendPortName == "" {
		if !routesAllHavePort {
			add("'backendPortName' cannot be empty")
		}
	} else if msgs := validation.IsValidPortName(bc.BackendPortName); len(msgs) > 0 {
		add("invalid backendPortName %q: %s", bc.BackendPortName, strings.Join(msgs, "; "))
	}

	// Rule 7: balancer
	errs = append(errs, balancerProblems("", bc.Balancer)...)

	// Rule 8: health (config-level and per-pool for the merged route view)
	errs = append(errs, healthProblems("health", bc.Health, bc.Protocol)...)

	// Rule 9: limits and timeouts
	errs = append(errs, limitsProblems("limits", bc.Limits)...)
	if bc.DialTimeout < 0 {
		add("invalid dialTimeout %s: must be >= 0 (0 uses the default)", bc.DialTimeout)
	}
	if bc.RequestTimeout < 0 {
		add("invalid requestTimeout %d: must be >= 0 (0 uses the default)", bc.RequestTimeout)
	}
	if bc.RequestTimeout > 0 && bc.DialTimeout.Std() != time.Duration(bc.RequestTimeout)*time.Second {
		add("requestTimeout (deprecated) %d conflicts with dialTimeout %s: remove requestTimeout", bc.RequestTimeout, bc.DialTimeout)
	}
	if bc.IdleTimeout < 0 {
		add("invalid idleTimeout %s: must be >= 0", bc.IdleTimeout)
	}

	// Rule 10: CIDRs and proxy protocol
	errs = append(errs, cidrProblems("access.allow", bc.Access.Allow)...)
	errs = append(errs, cidrProblems("access.deny", bc.Access.Deny)...)
	errs = append(errs, cidrProblems("proxyProtocol.in.trustedCIDRs", bc.ProxyProtocol.In.TrustedCIDRs)...)
	switch bc.ProxyProtocol.Out {
	case "", "v1", "v2":
	default:
		add("invalid proxyProtocol.out %q: must be \"\", v1 or v2", bc.ProxyProtocol.Out)
	}
	if bc.ProxyProtocol.In.Required && len(bc.ProxyProtocol.In.TrustedCIDRs) == 0 {
		add("proxyProtocol.in.required needs proxyProtocol.in.trustedCIDRs")
	}

	return errs
}

func (u *UDP) problems() []error {
	var errs []error
	if u.SessionIdleTimeout <= 0 {
		errs = append(errs, fmt.Errorf("invalid udp.sessionIdleTimeout %s: must be > 0", u.SessionIdleTimeout))
	}
	if u.MaxSessions < 0 {
		errs = append(errs, fmt.Errorf("invalid udp.maxSessions %d: must be >= 0", u.MaxSessions))
	}
	if u.MaxSessionsPerSource < 0 {
		errs = append(errs, fmt.Errorf("invalid udp.maxSessionsPerSource %d: must be >= 0", u.MaxSessionsPerSource))
	}
	if u.BufferSize < 512 || u.BufferSize > 65535 {
		errs = append(errs, fmt.Errorf("invalid udp.bufferSize %d: must be between 512 and 65535", u.BufferSize))
	}
	return errs
}

// tlsProblems validates the tls block; allHavePort reports whether every route
// sets its own backendPortName.
func (bc *Configuration) tlsProblems(allHavePort *bool) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	t := bc.TLS

	if t.PeekTimeout <= 0 {
		add("invalid tls.peekTimeout %s: must be > 0", t.PeekTimeout)
	}
	if t.MaxClientHello < 1 || t.MaxClientHello > 65535 {
		add("invalid tls.maxClientHello %d: must be between 1 and 65535", t.MaxClientHello)
	}
	if len(t.Routes) == 0 {
		add("protocol: tls requires tls.routes with at least one route")
		return errs
	}

	*allHavePort = true
	routeNames := make(map[string]int)
	hosts := make(map[string]string)
	pools := bc.Pools()

	for i, r := range t.Routes {
		rp := fmt.Sprintf("tls.routes[%d] (%s)", i, r.Name)
		if r.Name == "" {
			add("tls.routes[%d]: 'name' cannot be empty", i)
		} else if !namePattern.MatchString(r.Name) {
			add("%s: invalid name %q: must match %s", rp, r.Name, namePattern)
		}
		if r.Name != "" {
			if first, ok := routeNames[r.Name]; ok {
				add("%s: duplicate route name, already used by tls.routes[%d]", rp, first)
			} else {
				routeNames[r.Name] = i
			}
		}

		if len(r.Hosts) == 0 {
			add("%s: needs at least one host", rp)
		}
		for _, h := range r.Hosts {
			if h != strings.ToLower(h) {
				add("%s: host %q must be lowercase", rp, h)
				continue
			}
			var msgs []string
			if strings.HasPrefix(h, "*") {
				msgs = validation.IsWildcardDNS1123Subdomain(h)
			} else {
				msgs = validation.IsDNS1123Subdomain(h)
			}
			if len(msgs) > 0 {
				add("%s: invalid host %q: %s (a wildcard is only allowed as the leading \"*.\" label)", rp, h, strings.Join(msgs, "; "))
				continue
			}
			if other, dup := hosts[h]; dup {
				add("%s: host %q is already routed by %q", rp, h, other)
			} else {
				hosts[h] = r.Name
			}
		}

		if r.BackendPortName == "" {
			*allHavePort = false
		} else if msgs := validation.IsValidPortName(r.BackendPortName); len(msgs) > 0 {
			add("%s: invalid backendPortName %q: %s", rp, r.BackendPortName, strings.Join(msgs, "; "))
		}

		if r.Balancer != nil {
			errs = append(errs, balancerProblems(rp+": ", *r.Balancer)...)
		}
		if r.Limits != nil {
			if r.Limits.MaxConnections != 0 || r.Limits.MaxConnectionsPerSource != 0 {
				add("%s: limits: only maxConnectionsPerBackend can be overridden per route", rp)
			}
			if r.Limits.MaxConnectionsPerBackend < 0 {
				add("%s: invalid limits.maxConnectionsPerBackend %d: must be >= 0", rp, r.Limits.MaxConnectionsPerBackend)
			}
		}
		if r.Health != nil && i < len(pools) {
			for _, e := range healthProblems("health", pools[i].Health, bc.Protocol) {
				add("%s: %w", rp, e)
			}
		}
	}

	if t.DefaultRoute != "" {
		if _, ok := routeNames[t.DefaultRoute]; !ok {
			add("tls.defaultRoute %q does not name a route", t.DefaultRoute)
		}
	}
	return errs
}

func balancerProblems(prefix string, b Balancer) []error {
	var errs []error
	if b.Algorithm != "" && !slices.Contains(algorithms, b.Algorithm) {
		errs = append(errs, fmt.Errorf("%sinvalid balancer.algorithm %q: must be one of %s", prefix, b.Algorithm, strings.Join(algorithms, ", ")))
	}
	if b.SlowStart < 0 {
		errs = append(errs, fmt.Errorf("%sinvalid balancer.slowStart %s: must be >= 0", prefix, b.SlowStart))
	}
	return errs
}

func healthProblems(name string, h Health, proto Protocol) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(name+": "+format, a...)) }

	if !slices.Contains(healthTypes, h.Type) {
		add("invalid type %q: must be one of tcp, http, none", h.Type)
		return errs
	}
	if h.Port < 0 || h.Port > 65535 {
		add("invalid port %d: must be between 0 and 65535", h.Port)
	}
	if proto == ProtocolUDP && h.Type != HealthNone && h.Port == 0 {
		add("type %s on a udp configuration needs an explicit 'port' (UDP has no generic probe; use none for passive ejection only)", h.Type)
	}
	if h.Type == HealthNone {
		return errs
	}
	if h.Timeout <= 0 {
		add("invalid timeout %s: must be > 0", h.Timeout)
	}
	if h.Interval <= h.Timeout {
		add("invalid interval %s: must be greater than timeout %s", h.Interval, h.Timeout)
	}
	if h.Rise < 1 {
		add("invalid rise %d: must be >= 1", h.Rise)
	}
	if h.Fall < 1 {
		add("invalid fall %d: must be >= 1", h.Fall)
	}
	if h.EjectionHold < 0 {
		add("invalid ejectionHold %s: must be >= 0", h.EjectionHold)
	}
	if !(h.Jitter >= 0 && h.Jitter < 1) {
		add("invalid jitter %v: must be >= 0 and < 1", h.Jitter)
	}
	if h.Type == HealthHTTP {
		if !strings.HasPrefix(h.HTTP.Path, "/") {
			add("invalid http.path %q: must start with \"/\"", h.HTTP.Path)
		}
		for _, code := range h.HTTP.ExpectStatus {
			if code < 100 || code > 599 {
				add("invalid http.expectStatus %d: must be between 100 and 599", code)
			}
		}
	}
	return errs
}

func limitsProblems(name string, l ListenerLimits) []error {
	var errs []error
	for k, v := range map[string]int{
		"maxConnections":           l.MaxConnections,
		"maxConnectionsPerSource":  l.MaxConnectionsPerSource,
		"maxConnectionsPerBackend": l.MaxConnectionsPerBackend,
	} {
		if v < 0 {
			errs = append(errs, fmt.Errorf("invalid %s.%s %d: must be >= 0", name, k, v))
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

func cidrProblems(name string, cidrs []string) []error {
	var errs []error
	for _, c := range cidrs {
		if _, err := netip.ParsePrefix(c); err != nil {
			errs = append(errs, fmt.Errorf("invalid %s entry %q: %w", name, c, err))
		}
	}
	return errs
}

// namespaceProblems validates the merged Namespace and Namespaces entries.
func (bc *Configuration) namespaceProblems() []error {
	var errs []error

	entries := make([]string, 0, len(bc.Namespaces)+1)
	if bc.Namespace != "" {
		entries = append(entries, bc.Namespace)
	}
	for _, ns := range bc.Namespaces {
		if ns == "" {
			errs = append(errs, errors.New("invalid namespaces: entries must not be empty"))
			continue
		}
		entries = append(entries, ns)
	}

	if len(entries) == 0 && len(errs) == 0 {
		return []error{errors.New(`no namespaces configured: set "namespaces: [<namespace>]", or "namespaces: [\"*\"]" to discover Services cluster-wide`)}
	}

	hasAll := false
	others := 0
	for _, ns := range entries {
		if ns == AllNamespaces {
			hasAll = true
			continue
		}
		others++
		if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(msgs, "; ")))
		}
	}
	if hasAll && others > 0 {
		errs = append(errs, fmt.Errorf("invalid namespaces: %q must be the only entry, it cannot be combined with other namespaces", AllNamespaces))
	}
	return errs
}

// parseListenerAddress splits and validates a listenerAddress, returning its
// host (possibly empty) and port.
func parseListenerAddress(addr string) (string, int, error) {
	if addr == "" {
		return "", 0, errors.New("'listenerAddress' cannot be empty")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid listenerAddress %q: %w (write the port as \":8080\" or \"<ip>:8080\")", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid listenerAddress %q: port %q must be a number between 1 and 65535", addr, portStr)
	}
	if host != "" && net.ParseIP(host) == nil {
		return "", 0, fmt.Errorf("invalid listenerAddress %q: host %q must be empty or an IP address", addr, host)
	}
	return host, int(port), nil
}

// isWildcardHost reports whether a listener host binds every address.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// listenersConflict reports whether two listener addresses cannot be bound
// together: same port and the same host, or either host is a wildcard.
func listenersConflict(hostA string, portA int, hostB string, portB int) bool {
	if portA != portB {
		return false
	}
	if isWildcardHost(hostA) || isWildcardHost(hostB) {
		return true
	}
	return net.ParseIP(hostA).Equal(net.ParseIP(hostB))
}

// GetListenerPort extracts the port number from ListenerAddress.
func (bc *Configuration) GetListenerPort() (int, error) {
	_, port, err := net.SplitHostPort(strings.TrimSpace(bc.ListenerAddress))
	if err != nil {
		return 0, fmt.Errorf("invalid listenerAddress '%s': %w", bc.ListenerAddress, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, fmt.Errorf("invalid listenerAddress '%s': %w", bc.ListenerAddress, err)
	}
	return n, nil
}
