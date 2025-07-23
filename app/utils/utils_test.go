package utils

import (
	"os"
	"path/filepath"
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

func TestLoadConfigEmptyFile(t *testing.T) {
	// Create an empty config file
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "empty_config.yaml")

	err := os.WriteFile(configFile, []byte(""), 0644)
	if err != nil {
		t.Fatalf("Failed to create test config file: %v", err)
	}

	cfg, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("LoadConfig() failed for empty file: %v", err)
	}

	// Empty file should result in default values
	if len(cfg.BackendConfigurations) != 0 {
		t.Errorf("Expected 0 backend configurations for empty file, got %d", len(cfg.BackendConfigurations))
	}
}
