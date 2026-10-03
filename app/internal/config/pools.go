package config

import "time"

// EffectiveDialTimeout is DialTimeout defaulted to 5s and capped at MaxDialTimeout.
func (c *Configuration) EffectiveDialTimeout() time.Duration {
	d := c.DialTimeout.Std()
	if d <= 0 {
		d = defaultDialTimeout
	}
	return min(d, MaxDialTimeout)
}

// Pools returns one PoolSpec per tcp/udp configuration (Key = name) or per tls
// route (Key = name/route), route overrides merged over the configuration's.
// Call it after ApplyDefaults.
func (c *Configuration) Pools() []PoolSpec {
	ns := c.DiscoveryNamespaces()
	base := PoolSpec{
		ConfigName:               c.Name,
		BackendPortName:          c.BackendPortName,
		Protocol:                 c.Protocol,
		Balancer:                 c.Balancer,
		Health:                   c.Health.withHTTPDefaults(),
		MaxConnectionsPerBackend: c.Limits.MaxConnectionsPerBackend,
		Namespaces:               ns,
	}
	if c.Protocol != ProtocolTLS {
		base.Key = c.Name
		return []PoolSpec{base}
	}
	if c.TLS == nil {
		return nil
	}
	pools := make([]PoolSpec, 0, len(c.TLS.Routes))
	for _, r := range c.TLS.Routes {
		p := base
		p.Key = c.Name + "/" + r.Name
		p.Route = r.Name
		if r.BackendPortName != "" {
			p.BackendPortName = r.BackendPortName
		}
		if r.Balancer != nil {
			if r.Balancer.Algorithm != "" {
				p.Balancer.Algorithm = r.Balancer.Algorithm
			}
			if r.Balancer.SlowStart != 0 {
				p.Balancer.SlowStart = r.Balancer.SlowStart
			}
		}
		if r.Health != nil {
			p.Health = mergeHealth(p.Health, *r.Health)
		}
		if r.Limits != nil && r.Limits.MaxConnectionsPerBackend != 0 {
			p.MaxConnectionsPerBackend = r.Limits.MaxConnectionsPerBackend
		}
		pools = append(pools, p)
	}
	return pools
}

// mergeHealth overlays the non-zero fields of o on b.
func mergeHealth(b, o Health) Health {
	if o.Type != "" {
		b.Type = o.Type
	}
	if o.Interval != 0 {
		b.Interval = o.Interval
	}
	if o.Timeout != 0 {
		b.Timeout = o.Timeout
	}
	if o.EjectionHold != 0 {
		b.EjectionHold = o.EjectionHold
	}
	if o.Rise != 0 {
		b.Rise = o.Rise
	}
	if o.Fall != 0 {
		b.Fall = o.Fall
	}
	if o.Jitter != 0 {
		b.Jitter = o.Jitter
	}
	if o.Port != 0 {
		b.Port = o.Port
	}
	if o.HTTP.Path != "" {
		b.HTTP.Path = o.HTTP.Path
	}
	if o.HTTP.Host != "" {
		b.HTTP.Host = o.HTTP.Host
	}
	if o.HTTP.ExpectStatus != nil {
		b.HTTP.ExpectStatus = o.HTTP.ExpectStatus
	}
	return b.withHTTPDefaults()
}

// withHTTPDefaults fills the http probe defaults when the type is http.
func (h Health) withHTTPDefaults() Health {
	if h.Type == HealthHTTP {
		if h.HTTP.Path == "" {
			h.HTTP.Path = "/"
		}
		if len(h.HTTP.ExpectStatus) == 0 {
			h.HTTP.ExpectStatus = []int{200, 204}
		}
	}
	return h
}
