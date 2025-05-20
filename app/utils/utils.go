package utils

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"

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

		log.Printf("System | Loaded configuration: %s > %s", bc.Name, ExtractPort(bc.ListenerAddress))

	}

	return configData, nil

}
