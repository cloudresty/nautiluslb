package config

import (
	"fmt"
	"strconv"
	"strings"
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
	Name            string `yaml:"name"`
	ListenerAddress string `yaml:"listenerAddress"`
	RequestTimeout  int    `yaml:"requestTimeout,omitempty"`
	BackendPortName string `yaml:"backendPortName"`
	Namespace       string `yaml:"namespace,omitempty"`
}

// Validate validates the backend configuration.
func (bc *Configuration) Validate() error {

	if bc.Name == "" {
		return fmt.Errorf("'name' cannot be empty")
	}

	if bc.ListenerAddress == "" {
		return fmt.Errorf("'listenerAddress' cannot be empty")
	}

	if bc.BackendPortName == "" {
		return fmt.Errorf("'backendPortName' cannot be empty")
	}

	return nil

}

// GetListenerPort extracts the port number from ListenerAddress
func (bc *Configuration) GetListenerPort() (int, error) {

	addr := strings.TrimSpace(bc.ListenerAddress)
	addr = strings.TrimPrefix(addr, ":")
	port, err := strconv.Atoi(addr)
	if err != nil {
		return 0, fmt.Errorf("invalid listenerAddress '%s': %v", bc.ListenerAddress, err)
	}

	return port, nil

}
