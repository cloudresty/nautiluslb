package sni

import (
	"fmt"
	"strings"
)

// RouteHosts is the minimal router input: a route name and its host patterns.
type RouteHosts struct {
	Name  string
	Hosts []string
}

// Router maps server names to route names.
type Router struct {
	exact    map[string]string
	wildcard map[string]string // key: ".example.com" (suffix incl. leading dot)
	def      string
}

// NewRouter builds a router. Hosts are lowercased; "*.x" matches exactly one
// leading label. Duplicate hosts are an error.
func NewRouter(routes []RouteHosts, defaultRoute string) (*Router, error) {
	rt := &Router{exact: map[string]string{}, wildcard: map[string]string{}, def: defaultRoute}
	for _, route := range routes {
		for _, h := range route.Hosts {
			h = strings.TrimSpace(h)
			if h == "" {
				return nil, fmt.Errorf("sni: route %q has an empty host", route.Name)
			}
			// Hosts are normalised exactly like server names read from a
			// ClientHello, and the normalised form is the key, so a route
			// can never be configured that no ClientHello could match.
			m := rt.exact
			name, prefix := h, ""
			if strings.HasPrefix(h, "*.") {
				m, name, prefix = rt.wildcard, h[2:], "."
			}
			n, err := normalize(name)
			if err != nil {
				return nil, fmt.Errorf("sni: route %q has invalid host %q", route.Name, h)
			}
			key := prefix + n
			if prev, dup := m[key]; dup {
				return nil, fmt.Errorf("sni: host %q used by routes %q and %q", h, prev, route.Name)
			}
			m[key] = route.Name
		}
	}
	return rt, nil
}

// Match resolves a server name: exact, then one-label wildcard, then default.
func (r *Router) Match(serverName string) (string, bool) {
	s := strings.ToLower(strings.TrimSuffix(serverName, "."))
	if s != "" {
		if n, ok := r.exact[s]; ok {
			return n, true
		}
		if i := strings.IndexByte(s, '.'); i > 0 {
			if n, ok := r.wildcard[s[i:]]; ok {
				return n, true
			}
		}
	}
	if r.def != "" {
		return r.def, true
	}
	return "", false
}
