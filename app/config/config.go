package config

import "fmt"

// Config represents the overall configuration for the SLB.
type Config struct {
	Settings struct {
		KubeconfigPath string `yaml:"kubeconfig_path"`
	} `yaml:"settings"`
	BackendConfigurations []BackendConfiguration `yaml:"backendConfigurations"`
}

// BackendConfiguration represents the configuration for a backend.
type BackendConfiguration struct {
	Name                string `yaml:"name"`
	ListenerAddress     string `yaml:"listener_address"`
	HealthCheckInterval int    `yaml:"health_check_interval"`
	LabelSelector       string `yaml:"label_selector"`
	RequestTimeout      int    `yaml:"request_timeout,omitempty"`
}

// Validate validates the backend configuration.
func (bc *BackendConfiguration) Validate() error {

	if bc.HealthCheckInterval <= 0 {
		return fmt.Errorf("invalid health_check_interval: %d (must be positive)", bc.HealthCheckInterval)
	}

	if bc.ListenerAddress == "" {
		return fmt.Errorf("listener_address cannot be empty")
	}

	return nil
}
