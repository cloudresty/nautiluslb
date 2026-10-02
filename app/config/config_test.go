package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestConfigStructure(t *testing.T) {
	// Test that the Config struct can be instantiated
	config := &Config{}
	if len(config.BackendConfigurations) != 0 {
		t.Error("New Config should have empty BackendConfigurations")
	}
}

func TestConfigurationStructure(t *testing.T) {
	// Test that the Configuration struct can be instantiated
	config := &Configuration{}
	if config.Name != "" {
		t.Error("New Configuration should have empty Name")
	}
}

func TestGetListenerPort(t *testing.T) {
	config := &Configuration{
		ListenerAddress: ":8080",
	}

	port, err := config.GetListenerPort()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	expected := 8080
	if port != expected {
		t.Errorf("Expected port %d, got %d", expected, port)
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
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidateReportsAllProblems(t *testing.T) {
	cfg := Config{BackendConfigurations: []Configuration{
		{Name: "a b", ListenerAddress: "8080", BackendPortName: "http", Namespaces: []string{"default"}},
		{Name: "ok", ListenerAddress: ":80", BackendPortName: "BAD", RequestTimeout: -5},
	}}

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
	if err := (&Config{}).Validate(); err == nil {
		t.Fatal("expected error for zero configurations")
	}
}

func TestConfigValidateDuplicateNames(t *testing.T) {
	a, b := validConfiguration(), validConfiguration()
	b.ListenerAddress = ":9090"
	err := (&Config{BackendConfigurations: []Configuration{a, b}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "configurations[1] (web): duplicate name, already used by configurations[0]") {
		t.Fatalf("unexpected error: %v", err)
	}
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
			err := (&Config{BackendConfigurations: []Configuration{a, b}}).Validate()
			if tt.conflict {
				if err == nil || !strings.Contains(err.Error(), "conflicts with configurations[0]") {
					t.Fatalf("expected conflict, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
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
