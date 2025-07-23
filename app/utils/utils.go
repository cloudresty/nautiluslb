package utils

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/config"
	"gopkg.in/yaml.v3"
)

//
// ExtractPort extracts the port from a given address string.
//

func ExtractPort(addr string) string {

	_, port, err := net.SplitHostPort(addr)

	if err != nil {

		// If it fails, maybe it's just a port
		if strings.Contains(addr, ":") {
			return "" // invalid format
		}

		return addr

	}

	return port

}

//
// loadConfig reads the configuration from a YAML file and returns a Config struct.
//

func LoadConfig(filename string) (config.Config, error) {

	// Read the YAML file (config.yaml)
	data, err := os.ReadFile(filename)
	if err != nil {
		return config.Config{}, err
	}

	// Unmarshal the YAML data into the Config struct
	var configData config.Config
	err = yaml.Unmarshal(data, &configData)
	if err != nil {
		return config.Config{}, err
	}

	// Validate backend configurations
	for i, bc := range configData.BackendConfigurations {

		if err := bc.Validate(); err != nil {
			return config.Config{}, fmt.Errorf("invalid backend configuration at index %d: %v", i, err)
		}

		emit.Info.StructuredFields("Loaded configuration",
			emit.ZString("config_name", bc.Name),
			emit.ZString("listener_port", ExtractPort(bc.ListenerAddress)))

	}

	return configData, nil

}
