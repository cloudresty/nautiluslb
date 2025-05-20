package config

import (
	"fmt"
)

// Config represents the overall configuration for the SLB.
type Config struct {
	Settings struct {
		KubeconfigPath string `yaml:"kubeconfigPath"`
	} `yaml:"settings"`
	BackendConfigurations []Configuration `yaml:"configurations"`
}

// Configuration represents the configuration for a backend.
type Configuration struct {
	Name                 string `yaml:"name"`
	ListenerAddress      string `yaml:"listenerAddress"`
	RequestTimeout       int    `yaml:"requestTimeout,omitempty"`
	BackendLabelSelector string `yaml:"backendLabelSelector"`
	BackendPortName      string `yaml:"backendPortName"`
}

// Validate validates the backend configuration.
func (bc *Configuration) Validate() error {

	// log.Printf("Validating backend configuration '%s'", bc.Name)

	if bc.Name == "" {
		return fmt.Errorf("'name' cannot be empty")
	}

	if bc.ListenerAddress == "" {
		return fmt.Errorf("'listenerAddress' cannot be empty")
	}

	if bc.BackendLabelSelector == "" {
		return fmt.Errorf("'backendLabelSelector' cannot be empty")
	}

	if bc.BackendPortName == "" {
		return fmt.Errorf("'backendPortName' cannot be empty")
	}

	return nil

}
