package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/cloudresty/emit"
	"go.yaml.in/yaml/v3"
)

// UnmarshalYAML seeds the health jitter default (0.2) so that an explicit
// jitter: 0 is distinguishable from an absent key. It uses the callback form
// so strict (KnownFields) decoding keeps applying to the nested fields.
func (c *Configuration) UnmarshalYAML(unmarshal func(any) error) error {
	type plain Configuration // no methods: avoids recursion
	p := plain{}
	p.Health.Jitter = 0.2
	p.seeded = true
	if err := unmarshal(&p); err != nil {
		return err
	}
	*c = Configuration(p)
	return nil
}

// DefaultUDPMaxSessions is the per-listener UDP session cap applied when
// udp.maxSessions is absent. Each session pins a read buffer, a goroutine and a
// socket: memory is about maxSessions x (bufferSize + ~10KB).
const DefaultUDPMaxSessions = 4096

// UnmarshalYAML seeds the maxSessions default so that an explicit
// maxSessions: 0 (unlimited) is distinguishable from an absent key.
func (u *UDP) UnmarshalYAML(unmarshal func(any) error) error {
	type plain UDP
	p := plain{MaxSessions: DefaultUDPMaxSessions}
	if err := unmarshal(&p); err != nil {
		return err
	}
	*u = UDP(p)
	return nil
}

// Parse strictly decodes a single-document YAML configuration. It does not
// apply defaults, read the environment or validate; see Load.
func Parse(name string, data []byte) (*Config, error) {
	cfg := &Config{}
	cfg.seedBools()

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing %s: file is empty", name)
		}
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}

	// Refuse a second document: it would be silently ignored otherwise
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parsing %s: multiple YAML documents are not supported", name)
		}
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err == nil {
		scanBareDurations(&root, "", &cfg.parseWarning)
	}

	return cfg, nil
}

// durationKeys are the YAML keys whose value is a Duration.
var durationKeys = map[string]bool{
	"resyncPeriod": true, "debounce": true, "readinessDelay": true, "timeout": true,
	"ejectionHold": true, "slowStart": true, "dialTimeout": true, "idleTimeout": true,
	"peekTimeout": true, "sessionIdleTimeout": true, "interval": true,
}

// scanBareDurations records a deprecation for every duration key written as a
// bare integer (v0.x style seconds).
func scanBareDurations(n *yaml.Node, path string, out *[]string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			scanBareDurations(c, path, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			scanBareDurations(c, path+"["+strconv.Itoa(i)+"]", out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if path != "" {
				p = path + "." + k.Value
			}
			if durationKeys[k.Value] && v.Kind == yaml.ScalarNode && v.ShortTag() == "!!int" && v.Value != "0" {
				*out = append(*out, fmt.Sprintf("%s: bare integer duration %s (seconds) is deprecated: write %q", p, v.Value, v.Value+"s"))
				continue
			}
			scanBareDurations(v, p, out)
		}
	}
}

// Load reads the configuration file, applies defaults and NLB_* environment
// overrides, validates it and logs its deprecations.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the operator-supplied --config/NLB_CONFIG file
	if err != nil {
		return nil, err
	}

	cfg, err := Parse(path, data)
	if err != nil {
		return nil, err
	}

	cfg.ApplyDefaults()

	if err := cfg.ApplyEnv(os.LookupEnv); err != nil {
		return nil, fmt.Errorf("invalid environment for %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration in %s: %w", path, err)
	}

	for _, d := range cfg.Deprecations() {
		emit.Warn.StructuredFields("Deprecated configuration", emit.ZString("detail", d))
	}

	for _, bc := range cfg.Configurations {
		namespaces := strings.Join(bc.DiscoveryNamespaces(), ",")
		if namespaces == "" {
			namespaces = AllNamespaces
		}
		emit.Info.StructuredFields("Loaded configuration",
			emit.ZString("config_name", bc.Name),
			emit.ZString("protocol", string(bc.Protocol)),
			emit.ZString("listener_port", listenerPort(bc.ListenerAddress)),
			emit.ZString("namespaces", namespaces))
	}

	return cfg, nil
}

// listenerPort returns the port of a host:port listener address, for logging.
func listenerPort(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}
