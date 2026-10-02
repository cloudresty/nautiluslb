package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/cloudresty/emit"
	"go.yaml.in/yaml/v3"
)

//
// parseConfig strictly decodes and validates a single-document YAML configuration.
//

func parseConfig(filename string, data []byte) (Config, error) {

	var configData Config

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&configData); err != nil {

		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("parsing %s: file is empty", filename)
		}

		return Config{}, fmt.Errorf("parsing %s: %w", filename, err)

	}

	// Refuse a second document: it would be silently ignored otherwise
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {

		if err == nil {
			return Config{}, fmt.Errorf("parsing %s: multiple YAML documents are not supported", filename)
		}

		return Config{}, fmt.Errorf("parsing %s: %w", filename, err)

	}

	if err := configData.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid configuration in %s: %w", filename, err)
	}

	return configData, nil

}

//
// Load reads the configuration from a YAML file and returns a Config struct.
//

func Load(filename string) (Config, error) {

	// Read the YAML file (config.yaml)
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, err
	}

	configData, err := parseConfig(filename, data)
	if err != nil {
		return Config{}, err
	}

	for _, bc := range configData.BackendConfigurations {

		namespaces := strings.Join(bc.DiscoveryNamespaces(), ",")
		if namespaces == "" {
			namespaces = AllNamespaces
		}

		emit.Info.StructuredFields("Loaded configuration",
			emit.ZString("config_name", bc.Name),
			emit.ZString("listener_port", listenerPort(bc.ListenerAddress)),
			emit.ZString("namespaces", namespaces))

	}

	return configData, nil

}

// listenerPort returns the port of a host:port listener address, for logging.
func listenerPort(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}
