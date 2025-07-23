package config

import (
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

func TestGetListenerPortWithoutColon(t *testing.T) {
	config := &Configuration{
		ListenerAddress: "8080",
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
