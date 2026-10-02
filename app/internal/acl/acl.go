// Package acl implements CIDR allow/deny lists for client addresses.
package acl

import (
	"fmt"
	"net/netip"
	"strings"
)

// List is an immutable allow/deny list. A nil *List allows everything.
type List struct {
	allow []netip.Prefix
	deny  []netip.Prefix
}

func parseAll(kind string, in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("invalid %s entry %q: not a CIDR or IP", kind, s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		// Normalise IPv4-mapped prefixes so they match unmapped addresses.
		if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Parse builds a List from CIDR or bare-IP entries. Deny wins over allow;
// an empty allow list allows all addresses not denied.
func Parse(allow, deny []string) (*List, error) {
	a, err := parseAll("allow", allow)
	if err != nil {
		return nil, err
	}
	d, err := parseAll("deny", deny)
	if err != nil {
		return nil, err
	}
	return &List{allow: a, deny: d}, nil
}

// Empty reports whether the list has no rules.
func (l *List) Empty() bool { return l == nil || (len(l.allow) == 0 && len(l.deny) == 0) }

// Allowed reports whether ip may connect. It does not allocate.
func (l *List) Allowed(ip netip.Addr) bool {
	if l == nil {
		return true
	}
	ip = ip.Unmap().WithZone("")
	for _, p := range l.deny {
		if p.Contains(ip) {
			return false
		}
	}
	if len(l.allow) == 0 {
		return true
	}
	for _, p := range l.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
