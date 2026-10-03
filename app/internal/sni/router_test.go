package sni

import "testing"

func TestRouterWildcardOneLabel(t *testing.T) {
	r, err := NewRouter([]RouteHosts{{"w", []string{"*.Example.com"}}, {"x", []string{"EXACT.example.com"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"a.example.com": "w", "A.EXAMPLE.com.": "w", "exact.example.com": "x"}
	for in, want := range cases {
		if got, ok := r.Match(in); !ok || got != want {
			t.Fatalf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"a.b.example.com", "example.com", "", "other.org"} {
		if got, ok := r.Match(in); ok {
			t.Fatalf("%q matched %q", in, got)
		}
	}
}

func TestRouterDefault(t *testing.T) {
	r, _ := NewRouter([]RouteHosts{{"a", []string{"a.com"}}}, "dflt")
	if got, ok := r.Match("a.com"); !ok || got != "a" {
		t.Fatal(got)
	}
	for _, in := range []string{"zzz.com", ""} {
		if got, ok := r.Match(in); !ok || got != "dflt" {
			t.Fatalf("%q: %q %v", in, got, ok)
		}
	}
}

func TestRouterDuplicateHost(t *testing.T) {
	for _, rs := range [][]RouteHosts{
		{{"a", []string{"x.com"}}, {"b", []string{"X.com"}}},
		{{"a", []string{"*.x.com"}}, {"b", []string{"*.x.com"}}},
		{{"a", []string{"x.com", "x.com"}}},
	} {
		if _, err := NewRouter(rs, ""); err == nil {
			t.Fatalf("expected error for %v", rs)
		}
	}
	for _, h := range []string{"", "*", "*.", "a.*.com", "**.com"} {
		if _, err := NewRouter([]RouteHosts{{"a", []string{h}}}, ""); err == nil {
			t.Fatalf("expected error for %q", h)
		}
	}
}

// Hosts that no ClientHello could carry are rejected when the router is
// built, instead of becoming routes that never match (found by FuzzRouterMatch).
func TestRouterRejectsMalformedHosts(t *testing.T) {
	for _, h := range []string{"..", ".", "00..", "a..b", "-a.example", "a b.example", "*.", "*..example", "*.a..b", "a.*.example"} {
		if _, err := NewRouter([]RouteHosts{{Name: "r", Hosts: []string{h}}}, ""); err == nil {
			t.Errorf("host %q accepted", h)
		}
	}
	for _, h := range []string{"example.com", "Example.COM.", "*.example.com", "a_b.example"} {
		if _, err := NewRouter([]RouteHosts{{Name: "r", Hosts: []string{h}}}, ""); err != nil {
			t.Errorf("host %q rejected: %v", h, err)
		}
	}
}
