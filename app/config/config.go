package config

import (
	"fmt"
	"sort"
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

// ServiceEnabledAnnotation marks a Service as a NautilusLB backend.
const ServiceEnabledAnnotation = "nautiluslb.cloudresty.io/enabled"

// ServiceConfigurationsAnnotation binds a Service to configurations by name,
// as a comma-separated list. A Service is a backend of a configuration only
// when it names it here: matching on the port name alone would let any
// annotated Service with a port called "https" join that listener's pool.
const ServiceConfigurationsAnnotation = "nautiluslb.cloudresty.io/configurations"

// AllNamespaces in Namespaces opts a configuration into cluster-wide
// discovery. It must be the only entry.
const AllNamespaces = "*"

// Configuration represents the configuration for a backend.
type Configuration struct {
	Name            string `yaml:"name"`
	ListenerAddress string `yaml:"listenerAddress"`
	RequestTimeout  int    `yaml:"requestTimeout,omitempty"`
	BackendPortName string `yaml:"backendPortName"`

	// Namespace is the single-namespace form, kept for existing configs.
	// It is merged with Namespaces.
	Namespace string `yaml:"namespace,omitempty"`

	// Namespaces is the allowlist of namespaces searched for Services.
	// At least one namespace (or AllNamespaces) is required: an empty list
	// is refused rather than read as cluster-wide.
	Namespaces []string `yaml:"namespaces,omitempty"`
}

// DiscoveryNamespaces returns the namespaces to list Services in: Namespace
// and Namespaces merged, deduplicated and sorted. Cluster-wide discovery is
// returned as a single metav1.NamespaceAll (""). Call it only on a validated
// configuration.
func (bc *Configuration) DiscoveryNamespaces() []string {
	seen := make(map[string]bool)
	var namespaces []string
	for _, ns := range append([]string{bc.Namespace}, bc.Namespaces...) {
		ns = strings.TrimSpace(ns)
		if ns == "" || seen[ns] {
			continue
		}
		if ns == AllNamespaces {
			return []string{""}
		}
		seen[ns] = true
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return namespaces
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
