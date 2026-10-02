package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractPort(t *testing.T) {
	tests := []struct{ name, addr, expected string }{
		{"Standard host:port format", "localhost:8080", "8080"},
		{"IP:port format", "192.168.1.1:9090", "9090"},
		{"Just port number", "3000", ""},
		{"Port with colon prefix", ":5432", "5432"},
		{"IPv6 format", "[::1]:8080", "8080"},
		{"Empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := listenerPort(tt.addr); result != tt.expected {
				t.Errorf("listenerPort(%q) = %q; want %q", tt.addr, result, tt.expected)
			}
		})
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}
	return f
}

const header = "apiVersion: nautiluslb.cloudresty.io/v2\nkind: Config\n"

const validDoc = header + `configurations:
  - name: web
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [default]
`

func TestLoadConfig(t *testing.T) {
	// v2 file using the deprecated aliases: still loads and is mapped
	f := writeTemp(t, header+`settings:
  kubeconfigPath: "/test/path"
configurations:
  - name: "test_config"
    listenerAddress: ":8080"
    requestTimeout: 3
    backendPortName: "http"
    namespace: "default"
`)
	t.Setenv("NLB_KUBECONFIG", "")
	_ = os.Unsetenv("NLB_KUBECONFIG") // restored by t.Setenv cleanup
	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Settings.Kubernetes.Kubeconfig != "/test/path" {
		t.Errorf("kubeconfig = %q", cfg.Settings.Kubernetes.Kubeconfig)
	}
	if len(cfg.Configurations) != 1 {
		t.Fatalf("expected 1 configuration, got %d", len(cfg.Configurations))
	}
	bc := cfg.Configurations[0]
	if bc.Name != "test_config" || bc.ListenerAddress != ":8080" || bc.DialTimeout.Std() != 3*time.Second {
		t.Errorf("unexpected configuration %+v", bc)
	}
	if len(cfg.Deprecations()) != 3 {
		t.Errorf("deprecations = %v; want 3", cfg.Deprecations())
	}
}

func TestLoadConfigFileNotFound(t *testing.T) {
	if _, err := Load("nonexistent_file.yaml"); err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	f := writeTemp(t, header+`configurations:
  - name: "test_config"
    namespace: [invalid yaml structure
`)
	if _, err := Load(f); err == nil {
		t.Error("Expected error for invalid YAML, got nil")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"empty file", "", "file is empty"},
		{"whitespace only", "\n\n", "file is empty"},
		{"null document", "---\n", "no configurations defined"},
		{"no configurations", header + "settings:\n  kubeconfigPath: \"\"\n", "no configurations defined"},
		{"namespce typo", header + `configurations:
  - name: web
    listenerAddress: ":8080"
    backendPortName: http
    namespce: default
`, "field namespce not found"},
		{"unknown top-level key", validDoc + "extra: 1\n", "field extra not found"},
		{"multiple documents", validDoc + "---\n" + validDoc, "multiple YAML documents"},
		{"missing namespaces", header + `configurations:
  - name: web
    listenerAddress: ":8080"
    backendPortName: http
`, "no namespaces configured"},
		{"duplicate names", validDoc + `  - name: web
    listenerAddress: ":9090"
    backendPortName: http
    namespaces: [default]
`, "duplicate name"},
		{"listener conflict", validDoc + `  - name: other
    listenerAddress: "10.0.0.1:8080"
    backendPortName: http
    namespaces: [default]
`, "conflicts with configurations[0]"},
		{"nested unknown key", validDoc + `    health:
      intervall: 5s
`, "field intervall not found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := writeTemp(t, tt.content)
			_, err := Load(f)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want containing %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), f) {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestLoadTestdata(t *testing.T) {
	t.Run("v1 rejected with upgrade hint", func(t *testing.T) {
		_, err := Load(filepath.Join("testdata", "v1.yaml"))
		expectErr(t, err, "docs/upgrading-v2.md")
		expectErr(t, err, "v1 file")
	})
	t.Run("minimal", func(t *testing.T) {
		cfg, err := Load(filepath.Join("testdata", "minimal-v2.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Deprecations()) != 0 {
			t.Errorf("unexpected deprecations %v", cfg.Deprecations())
		}
	})
	t.Run("full", func(t *testing.T) {
		cfg, err := Load(filepath.Join("testdata", "full-v2.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		s := cfg.Settings
		if s.Admin.Address != "" || !s.Admin.Pprof || s.Admin.Metrics.PerBackend ||
			s.AccessLog.Enabled || s.Discovery.Nodes.ReadyOnly || s.Discovery.Nodes.SkipUnschedulable ||
			!s.Discovery.Nodes.SkipControlPlane || s.Discovery.Nodes.Selector["pool"] != "edge" ||
			s.Drain.Timeout.Std() != 45*time.Second || s.Kubernetes.QPS != 50 {
			t.Errorf("full settings not honoured: %+v", s)
		}
		h := cfg.Configurations[0].Health
		if h.Jitter != 0 || h.Rise != 2 || h.Interval.Std() != 5*time.Second {
			t.Errorf("explicit health (jitter 0) not honoured: %+v", h)
		}
		if got := cfg.Configurations[1].Health.Type; got != HealthTCP {
			t.Errorf("udp health type = %q", got)
		}
	})
}

func TestLoadExample(t *testing.T) {
	t.Setenv("NLB_ADMIN_ADDRESS", "127.0.0.1:9090")
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("Load(config.example.yaml) failed: %v", err)
	}
	if len(cfg.Configurations) != 4 {
		t.Errorf("expected 4 configurations, got %d", len(cfg.Configurations))
	}
	if d := cfg.Deprecations(); len(d) != 0 {
		t.Errorf("example must not use deprecated keys: %v", d)
	}
	var keys []string
	for _, c := range cfg.Configurations {
		for _, p := range c.Pools() {
			keys = append(keys, p.Key)
		}
	}
	if got := strings.Join(keys, ","); got != "https/web,https/api,http,mongodb,dns" {
		t.Errorf("pool keys = %s", got)
	}
}

func TestLoadEnvOverridesAndErrors(t *testing.T) {
	f := writeTemp(t, validDoc)
	t.Setenv("NLB_LOG_LEVEL", "debug")
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Settings.LogLevel != "debug" {
		t.Errorf("env override lost: %q", cfg.Settings.LogLevel)
	}
	t.Setenv("NLB_MAX_CONNECTIONS", "many")
	_, err = Load(f)
	expectErr(t, err, "NLB_MAX_CONNECTIONS")
}
