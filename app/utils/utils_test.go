package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractPort(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		expected string
	}{
		{
			name:     "Standard host:port format",
			addr:     "localhost:8080",
			expected: "8080",
		},
		{
			name:     "IP:port format",
			addr:     "192.168.1.1:9090",
			expected: "9090",
		},
		{
			name:     "Just port number",
			addr:     "3000",
			expected: "3000",
		},
		{
			name:     "Port with colon prefix",
			addr:     ":5432",
			expected: "5432",
		},
		{
			name:     "IPv6 format",
			addr:     "[::1]:8080",
			expected: "8080",
		},
		{
			name:     "Empty string",
			addr:     "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractPort(tt.addr)
			if result != tt.expected {
				t.Errorf("ExtractPort(%q) = %q; want %q", tt.addr, result, tt.expected)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "test_config.yaml")

	configContent := `settings:
  kubeconfigPath: "/test/path"
configurations:
  - name: "test_config"
    listenerAddress: ":8080"
    requestTimeout: 30
    backendPortName: "http"
    namespace: "default"
`

	err := os.WriteFile(configFile, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}

	// Test loading the config
	cfg, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}

	// Verify the config was loaded correctly
	if cfg.Settings.KubeconfigPath != "/test/path" {
		t.Errorf("Expected kubeconfigPath '/test/path', got '%s'", cfg.Settings.KubeconfigPath)
	}

	if len(cfg.BackendConfigurations) != 1 {
		t.Errorf("Expected 1 backend configuration, got %d", len(cfg.BackendConfigurations))
	}

	if cfg.BackendConfigurations[0].Name != "test_config" {
		t.Errorf("Expected name 'test_config', got '%s'", cfg.BackendConfigurations[0].Name)
	}

	if cfg.BackendConfigurations[0].ListenerAddress != ":8080" {
		t.Errorf("Expected listenerAddress ':8080', got '%s'", cfg.BackendConfigurations[0].ListenerAddress)
	}
}

func TestLoadConfigFileNotFound(t *testing.T) {
	_, err := LoadConfig("nonexistent_file.yaml")
	if err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	// Create a temporary file with invalid YAML
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "invalid_config.yaml")

	invalidContent := `settings:
  kubeconfigPath: "/test/path"
configurations:
  - name: "test_config"
    listenerAddress: ":8080"
    requestTimeout: 30
    backendPortName: "http"
    namespace: [invalid yaml structure
`

	err := os.WriteFile(configFile, []byte(invalidContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}

	_, err = LoadConfig(configFile)
	if err == nil {
		t.Error("Expected error for invalid YAML, got nil")
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

const validDoc = `configurations:
  - name: web
    listenerAddress: ":8080"
    backendPortName: http
    namespaces: [default]
`

func TestLoadConfigRejects(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"empty file", "", "file is empty"},
		{"whitespace only", "\n\n", "file is empty"},
		{"null document", "---\n", "no configurations defined"},
		{"no configurations", "settings:\n  kubeconfigPath: \"\"\n", "no configurations defined"},
		{"namespce typo", `configurations:
  - name: web
    listenerAddress: ":8080"
    backendPortName: http
    namespce: default
`, "field namespce not found"},
		{"unknown top-level key", validDoc + "extra: 1\n", "field extra not found"},
		{"multiple documents", validDoc + "---\n" + validDoc, "multiple YAML documents"},
		{"missing namespaces", `configurations:
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := writeTemp(t, tt.content)
			_, err := LoadConfig(f)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want containing %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), f) {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestLoadConfigExample(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig(config.example.yaml) failed: %v", err)
	}
	if len(cfg.BackendConfigurations) != 3 {
		t.Errorf("expected 3 configurations, got %d", len(cfg.BackendConfigurations))
	}
}

func FuzzLoadConfig(f *testing.F) {
	f.Add([]byte(validDoc))
	f.Add([]byte(""))
	f.Add([]byte("---\n"))
	f.Add([]byte(validDoc + "---\n" + validDoc))
	f.Add([]byte("configurations: [1, 2"))
	f.Add([]byte("configurations:\n  - name: a\n    namespces: x\n"))
	if data, err := os.ReadFile(filepath.Join("..", "config.example.yaml")); err == nil {
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := parseConfig("fuzz.yaml", data)
		if err != nil {
			return
		}
		// Anything accepted must be fully usable by discovery
		for _, bc := range cfg.BackendConfigurations {
			if len(bc.DiscoveryNamespaces()) == 0 {
				t.Fatalf("accepted configuration %q with no discovery namespaces", bc.Name)
			}
			if _, err := bc.GetListenerPort(); err != nil {
				t.Fatalf("accepted configuration %q with bad listener: %v", bc.Name, err)
			}
		}
	})
}
