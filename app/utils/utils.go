package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/cloudresty/emit"
	"github.com/cloudresty/nautiluslb/config"
	"go.yaml.in/yaml/v3"
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
// parseConfig strictly decodes and validates a single-document YAML configuration.
//

func parseConfig(filename string, data []byte) (config.Config, error) {

	var configData config.Config

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&configData); err != nil {

		if errors.Is(err, io.EOF) {
			return config.Config{}, fmt.Errorf("parsing %s: file is empty", filename)
		}

		return config.Config{}, fmt.Errorf("parsing %s: %w", filename, err)

	}

	// Refuse a second document: it would be silently ignored otherwise
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {

		if err == nil {
			return config.Config{}, fmt.Errorf("parsing %s: multiple YAML documents are not supported", filename)
		}

		return config.Config{}, fmt.Errorf("parsing %s: %w", filename, err)

	}

	if err := configData.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("invalid configuration in %s: %w", filename, err)
	}

	return configData, nil

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

	configData, err := parseConfig(filename, data)
	if err != nil {
		return config.Config{}, err
	}

	for _, bc := range configData.BackendConfigurations {

		namespaces := strings.Join(bc.DiscoveryNamespaces(), ",")
		if namespaces == "" {
			namespaces = config.AllNamespaces
		}

		emit.Info.StructuredFields("Loaded configuration",
			emit.ZString("config_name", bc.Name),
			emit.ZString("listener_port", ExtractPort(bc.ListenerAddress)),
			emit.ZString("namespaces", namespaces))

	}

	return configData, nil

}
