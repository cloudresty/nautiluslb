package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfigStructure(t *testing.T) {
	config := &Config{}
	if len(config.Configurations) != 0 {
		t.Error("New Config should have empty Configurations")
	}
}

func TestConfigurationStructure(t *testing.T) {
	config := &Configuration{}
	if config.Name != "" {
		t.Error("New Configuration should have empty Name")
	}
}

func TestGetListenerPort(t *testing.T) {
	config := &Configuration{ListenerAddress: ":8080"}
	port, err := config.GetListenerPort()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if port != 8080 {
		t.Errorf("Expected port 8080, got %d", port)
	}
}

func TestGetListenerPortInvalid(t *testing.T) {
	for _, addr := range []string{"8080", "", ":abc"} {
		t.Run(addr, func(t *testing.T) {
			c := &Configuration{ListenerAddress: addr}
			if _, err := c.GetListenerPort(); err == nil {
				t.Errorf("GetListenerPort(%q) expected error", addr)
			}
		})
	}
}

func validConfiguration() Configuration {
	return Configuration{
		Name:            "web",
		ListenerAddress: ":8080",
		BackendPortName: "http",
		Namespaces:      []string{"default"},
	}
}

func validConfig(cs ...Configuration) *Config {
	if len(cs) == 0 {
		cs = []Configuration{validConfiguration()}
	}
	return &Config{APIVersion: APIVersion, Kind: Kind, Configurations: cs}
}

// expectErr asserts err contains want ("" = no error).
func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v; want containing %q", err, want)
	}
}

func TestConfigurationValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Configuration)
		wantErr string // empty means valid
	}{
		{"valid", func(c *Configuration) {}, ""},
		{"valid legacy namespace", func(c *Configuration) { c.Namespace = "kube-system"; c.Namespaces = nil }, ""},
		{"valid ip listener", func(c *Configuration) { c.ListenerAddress = "10.0.0.1:8080" }, ""},
		{"valid ipv6 listener", func(c *Configuration) { c.ListenerAddress = "[::1]:8080" }, ""},
		{"valid all namespaces", func(c *Configuration) { c.Namespaces = []string{"*"} }, ""},
		{"valid name punctuation", func(c *Configuration) { c.Name = "a.b_c-d" }, ""},
		{"valid timeout", func(c *Configuration) { c.RequestTimeout = 30 }, ""},
		{"empty name", func(c *Configuration) { c.Name = "" }, "'name' cannot be empty"},
		{"name with comma", func(c *Configuration) { c.Name = "a,b" }, "invalid name"},
		{"name with space", func(c *Configuration) { c.Name = "a b" }, "invalid name"},
		{"name with slash", func(c *Configuration) { c.Name = "a/b" }, "invalid name"},
		{"name leading dash", func(c *Configuration) { c.Name = "-a" }, "invalid name"},
		{"name too long", func(c *Configuration) { c.Name = strings.Repeat("a", 64) }, "invalid name"},
		{"empty listener", func(c *Configuration) { c.ListenerAddress = "" }, "'listenerAddress' cannot be empty"},
		{"bare port", func(c *Configuration) { c.ListenerAddress = "8080" }, `write the port as ":8080"`},
		{"port zero", func(c *Configuration) { c.ListenerAddress = ":0" }, "between 1 and 65535"},
		{"port too big", func(c *Configuration) { c.ListenerAddress = ":65536" }, "between 1 and 65535"},
		{"port not numeric", func(c *Configuration) { c.ListenerAddress = ":http" }, "between 1 and 65535"},
		{"port signed", func(c *Configuration) { c.ListenerAddress = ":+80" }, "between 1 and 65535"},
		{"hostname host", func(c *Configuration) { c.ListenerAddress = "localhost:80" }, "must be empty or an IP address"},
		{"empty backendPortName", func(c *Configuration) { c.BackendPortName = "" }, "'backendPortName' cannot be empty"},
		{"bad backendPortName", func(c *Configuration) { c.BackendPortName = "HTTP_1" }, "invalid backendPortName"},
		{"negative timeout", func(c *Configuration) { c.RequestTimeout = -1 }, "invalid requestTimeout -1"},
		{"no namespaces", func(c *Configuration) { c.Namespaces = nil }, `no namespaces configured: set "namespaces: [<namespace>]", or "namespaces: [\"*\"]" to discover Services cluster-wide`},
		{"empty namespace entry", func(c *Configuration) { c.Namespaces = []string{"default", ""} }, "entries must not be empty"},
		{"invalid namespace", func(c *Configuration) { c.Namespaces = []string{"Bad_NS"} }, `invalid namespace "Bad_NS"`},
		{"invalid legacy namespace", func(c *Configuration) { c.Namespace = "Bad_NS"; c.Namespaces = nil }, `invalid namespace "Bad_NS"`},
		{"star with others", func(c *Configuration) { c.Namespaces = []string{"*", "default"} }, "must be the only entry"},
		{"star with legacy", func(c *Configuration) { c.Namespace = "default"; c.Namespaces = []string{"*"} }, "must be the only entry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfiguration()
			tt.mutate(&c)
			expectErr(t, validConfig(c).Validate(), tt.wantErr)
		})
	}
}

func TestConfigValidateReportsAllProblems(t *testing.T) {
	cfg := validConfig(
		Configuration{Name: "a b", ListenerAddress: "8080", BackendPortName: "http", Namespaces: []string{"default"}},
		Configuration{Name: "ok", ListenerAddress: ":80", BackendPortName: "BAD", RequestTimeout: -5},
	)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{
		"configurations[0] (a b): invalid name",
		"configurations[0] (a b): invalid listenerAddress",
		"configurations[1] (ok): invalid backendPortName",
		"configurations[1] (ok): invalid requestTimeout",
		"configurations[1] (ok): no namespaces configured",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestConfigValidateNoConfigurations(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	c := &Config{APIVersion: APIVersion, Kind: Kind}
	expectErr(t, c.Validate(), "no configurations defined")
}

func TestConfigValidateDuplicateNames(t *testing.T) {
	a, b := validConfiguration(), validConfiguration()
	b.ListenerAddress = ":9090"
	expectErr(t, validConfig(a, b).Validate(), "configurations[1] (web): duplicate name, already used by configurations[0]")
}

func TestConfigValidateListenerConflicts(t *testing.T) {
	tests := []struct {
		a, b     string
		conflict bool
	}{
		{":80", ":80", true},
		{":80", ":81", false},
		{"10.0.0.1:80", "10.0.0.1:80", true},
		{"10.0.0.1:80", "10.0.0.2:80", false},
		{"10.0.0.1:80", ":80", true},
		{":80", "10.0.0.1:80", true},
		{"0.0.0.0:80", "10.0.0.1:80", true},
		{"10.0.0.1:80", "0.0.0.0:80", true},
		{"[::]:80", "10.0.0.1:80", true},
		{"[::]:80", ":80", true},
		{"0.0.0.0:80", "[::]:80", true},
		{"10.0.0.1:80", "10.0.0.2:81", false},
		{"0.0.0.0:80", ":81", false},
		{"[::1]:80", "[::1]:80", true},
		{"[::1]:80", "[::2]:80", false},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			a, b := validConfiguration(), validConfiguration()
			a.ListenerAddress = tt.a
			b.Name = "other"
			b.ListenerAddress = tt.b
			err := validConfig(a, b).Validate()
			if tt.conflict {
				expectErr(t, err, "conflicts with configurations[0]")
				return
			}
			expectErr(t, err, "")
		})
	}
}

func TestListenerConflictsPerProtocolFamily(t *testing.T) {
	tests := []struct {
		pa, pb   Protocol
		conflict bool
	}{
		{ProtocolTCP, ProtocolUDP, false},
		{ProtocolUDP, ProtocolUDP, true},
		{ProtocolTCP, ProtocolTCP, true},
		{ProtocolTLS, ProtocolTCP, true},
		{ProtocolTLS, ProtocolUDP, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.pa)+"_"+string(tt.pb), func(t *testing.T) {
			a, b := udpOrTLS(tt.pa, "a"), udpOrTLS(tt.pb, "b")
			err := validConfig(a, b).Validate()
			if tt.conflict {
				expectErr(t, err, "conflicts with configurations[0]")
				return
			}
			expectErr(t, err, "")
		})
	}
}

// udpOrTLS builds a valid configuration of the given protocol on :53/:443-style shared port 9000.
func udpOrTLS(p Protocol, name string) Configuration {
	c := validConfiguration()
	c.Name, c.Protocol, c.ListenerAddress = name, p, ":9000"
	if p == ProtocolTLS {
		c.TLS = &TLS{Routes: []Route{{Name: "r", Hosts: []string{"a.example.com"}}}}
	}
	if p == ProtocolUDP {
		c.Health = Health{Type: HealthNone}
	}
	return c
}

func TestDiscoveryNamespaces(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		list      []string
		want      []string
	}{
		{"legacy only", "default", nil, []string{"default"}},
		{"list only sorted", "", []string{"b", "a"}, []string{"a", "b"}},
		{"merged", "c", []string{"b", "a"}, []string{"a", "b", "c"}},
		{"deduped", "a", []string{"a", "b", "b"}, []string{"a", "b"}},
		{"star", "", []string{"*"}, []string{""}},
		{"star wins", "a", []string{"b", "*"}, []string{""}},
		{"none", "", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Configuration{Namespace: tt.namespace, Namespaces: tt.list}
			if got := c.DiscoveryNamespaces(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DiscoveryNamespaces() = %#v; want %#v", got, tt.want)
			}
		})
	}
}

func TestParseBinding(t *testing.T) {
	tests := []struct{ in, cfg, route string }{
		{"a", "a", ""},
		{"a/b", "a", "b"},
		{" a / b ", "a", "b"},
		{"a/b/c", "a", "b/c"},
		{"", "", ""},
		{"a/", "a", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c, r := ParseBinding(tt.in)
			if c != tt.cfg || r != tt.route {
				t.Errorf("ParseBinding(%q) = (%q,%q); want (%q,%q)", tt.in, c, r, tt.cfg, tt.route)
			}
		})
	}
}

func TestDurationYAML(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30s", 30 * time.Second, false},
		{"1m", time.Minute, false},
		{"200ms", 200 * time.Millisecond, false},
		{`"1h30m"`, 90 * time.Minute, false},
		{"5", 5 * time.Second, false},
		{"0", 0, false},
		{"-3s", -3 * time.Second, false},
		{"abc", 0, true},
		{"1.5", 0, true},
		{"[1]", 0, true},
		{"{a: 1}", 0, true},
		{"99999999999999999", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			cfg, err := Parse("d.yaml", []byte("settings:\n  drain:\n    timeout: "+tt.in+"\n"))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", cfg.Settings.Drain.Timeout)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Settings.Drain.Timeout.Std(); got != tt.want {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}

	out, err := Duration(90 * time.Second).MarshalYAML()
	if err != nil || out != "1m30s" {
		t.Errorf("MarshalYAML = %v, %v", out, err)
	}
}
