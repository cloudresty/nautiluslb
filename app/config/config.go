package config

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
