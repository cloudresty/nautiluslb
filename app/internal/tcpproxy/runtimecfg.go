package tcpproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/cloudresty/nautiluslb/internal/acl"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/proxyproto"
	"github.com/cloudresty/nautiluslb/internal/sni"
)

const (
	defaultPeekTimeout    = 5 * time.Second
	defaultMaxClientHello = 16384
)

// runtimeConfig is everything Update may change. A connection loads it once
// and keeps it for its whole life, so a reload never changes a running
// connection's rules.
type runtimeConfig struct {
	cfg           config.Configuration
	acl           *acl.List
	trusted       []netip.Prefix
	proxyRequired bool
	proxyOut      proxyproto.Version // 0 = none
	tls           bool
	router        *sni.Router
	routeKeys     map[string]string // route name -> pool key
	peekTimeout   time.Duration
	maxHello      int
	dialTimeout   time.Duration
	idleTimeout   time.Duration
	pools         map[string]Pool
	poolKey       string // tcp: the single pool key
}

func normProtocol(p config.Protocol) config.Protocol {
	if p == "" {
		return config.ProtocolTCP
	}
	return p
}

func buildRuntimeConfig(cfg config.Configuration, pools map[string]Pool) (*runtimeConfig, error) {
	proto := normProtocol(cfg.Protocol)
	if proto == config.ProtocolUDP {
		return nil, fmt.Errorf("tcpproxy: listener %q: protocol udp is not served by tcpproxy", cfg.Name)
	}

	list, err := acl.Parse(cfg.Access.Allow, cfg.Access.Deny)
	if err != nil {
		return nil, fmt.Errorf("parsing access lists: %w", err)
	}

	rc := &runtimeConfig{
		cfg:           cfg,
		acl:           list,
		proxyRequired: cfg.ProxyProtocol.In.Required,
		tls:           proto == config.ProtocolTLS,
		dialTimeout:   cfg.EffectiveDialTimeout(),
		idleTimeout:   cfg.IdleTimeout.Std(),
		pools:         pools,
		poolKey:       cfg.Name,
	}

	for _, s := range cfg.ProxyProtocol.In.TrustedCIDRs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("invalid proxyProtocol.in.trustedCIDRs entry %q: %w", s, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		rc.trusted = append(rc.trusted, p.Masked())
	}

	switch cfg.ProxyProtocol.Out {
	case "":
	case "v1":
		rc.proxyOut = proxyproto.V1
	case "v2":
		rc.proxyOut = proxyproto.V2
	default:
		return nil, fmt.Errorf("invalid proxyProtocol.out %q", cfg.ProxyProtocol.Out)
	}

	if rc.tls {
		if cfg.TLS == nil {
			return nil, errors.New("protocol tls requires a tls section")
		}
		hosts := make([]sni.RouteHosts, 0, len(cfg.TLS.Routes))
		rc.routeKeys = make(map[string]string, len(cfg.TLS.Routes))
		for _, r := range cfg.TLS.Routes {
			hosts = append(hosts, sni.RouteHosts{Name: r.Name, Hosts: r.Hosts})
			rc.routeKeys[r.Name] = cfg.Name + "/" + r.Name
		}
		if rc.router, err = sni.NewRouter(hosts, cfg.TLS.DefaultRoute); err != nil {
			return nil, fmt.Errorf("building SNI router: %w", err)
		}
		rc.peekTimeout = cfg.TLS.PeekTimeout.Std()
		if rc.peekTimeout <= 0 {
			rc.peekTimeout = defaultPeekTimeout
		}
		rc.maxHello = cfg.TLS.MaxClientHello
		if rc.maxHello <= 0 {
			rc.maxHello = defaultMaxClientHello
		}
	}

	return rc, nil
}

func (rc *runtimeConfig) isTrusted(ip netip.Addr) bool {
	for _, p := range rc.trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
