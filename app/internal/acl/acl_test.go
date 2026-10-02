package acl

import (
	"net/netip"
	"testing"
)

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestACLDenyWins(t *testing.T) {
	l, err := Parse([]string{"10.0.0.0/8"}, []string{"10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Allowed(ip("10.2.0.1")) || l.Allowed(ip("10.1.0.1")) || l.Allowed(ip("192.168.0.1")) {
		t.Fatal("wrong decisions")
	}
}

func TestACLEmptyAllowsAll(t *testing.T) {
	l, _ := Parse(nil, nil)
	if !l.Empty() || !l.Allowed(ip("1.2.3.4")) {
		t.Fatal("empty list should allow all")
	}
	l, _ = Parse(nil, []string{"1.2.3.4"})
	if l.Empty() || l.Allowed(ip("1.2.3.4")) || !l.Allowed(ip("1.2.3.5")) {
		t.Fatal("deny-only wrong")
	}
}

func TestACLIPv6(t *testing.T) {
	l, _ := Parse([]string{"2001:db8::/32"}, nil)
	if !l.Allowed(ip("2001:db8::1")) || l.Allowed(ip("2001:db9::1")) {
		t.Fatal("ipv6 wrong")
	}
}

func TestACLBareIP(t *testing.T) {
	l, err := Parse([]string{"192.0.2.7", "2001:db8::7"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Allowed(ip("192.0.2.7")) || l.Allowed(ip("192.0.2.8")) ||
		!l.Allowed(ip("2001:db8::7")) || l.Allowed(ip("2001:db8::8")) {
		t.Fatal("bare ip wrong")
	}
}

func TestACLInvalid(t *testing.T) {
	if _, err := Parse([]string{"garbage"}, nil); err == nil {
		t.Fatal("expected error")
	} else if !contains(err.Error(), "garbage") {
		t.Fatalf("error should name entry: %v", err)
	}
	if _, err := Parse(nil, []string{"10.0.0.0/99"}); err == nil {
		t.Fatal("expected error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestACLNilAllows(t *testing.T) {
	var l *List
	if !l.Allowed(ip("1.1.1.1")) || !l.Empty() {
		t.Fatal("nil list should allow all")
	}
}

func TestACLMapped(t *testing.T) {
	l, _ := Parse([]string{"10.0.0.0/8"}, []string{"10.9.9.9"})
	if !l.Allowed(ip("::ffff:10.1.1.1")) || l.Allowed(ip("::ffff:10.9.9.9")) || l.Allowed(ip("::ffff:11.0.0.1")) {
		t.Fatal("mapped wrong")
	}
	l, _ = Parse([]string{"::ffff:10.0.0.0/104"}, nil)
	if !l.Allowed(ip("10.1.1.1")) {
		t.Fatal("mapped prefix not normalised")
	}
}

func BenchmarkACLAllowed(b *testing.B) {
	l, _ := Parse([]string{"10.0.0.0/8", "172.16.0.0/12", "2001:db8::/32"}, []string{"10.1.0.0/16"})
	a := ip("::ffff:10.2.3.4")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Allowed(a)
	}
}
