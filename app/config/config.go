package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// namePattern restricts configuration names: they are referenced inside a
// comma-separated annotation, so commas and spaces must be impossible.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

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

// Validate validates the backend configuration, reporting every problem found.
func (bc *Configuration) Validate() error {

	return errors.Join(bc.problems()...)

}

// problems collects every validation failure of a single configuration.
func (bc *Configuration) problems() []error {

	var errs []error

	if bc.Name == "" {
		errs = append(errs, errors.New("'name' cannot be empty"))
	} else if !namePattern.MatchString(bc.Name) {
		errs = append(errs, fmt.Errorf("invalid name %q: must match %s (it is referenced in a comma-separated annotation)", bc.Name, namePattern))
	}

	if _, _, err := parseListenerAddress(bc.ListenerAddress); err != nil {
		errs = append(errs, err)
	}

	if bc.BackendPortName == "" {
		errs = append(errs, errors.New("'backendPortName' cannot be empty"))
	} else if msgs := validation.IsValidPortName(bc.BackendPortName); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("invalid backendPortName %q: %s", bc.BackendPortName, strings.Join(msgs, "; ")))
	}

	if bc.RequestTimeout < 0 {
		errs = append(errs, fmt.Errorf("invalid requestTimeout %d: must be >= 0 (0 uses the default)", bc.RequestTimeout))
	}

	errs = append(errs, bc.namespaceProblems()...)

	return errs

}

// namespaceProblems validates the merged Namespace and Namespaces entries.
func (bc *Configuration) namespaceProblems() []error {

	var errs []error

	entries := make([]string, 0, len(bc.Namespaces)+1)
	if bc.Namespace != "" {
		entries = append(entries, bc.Namespace)
	}

	for _, ns := range bc.Namespaces {
		if ns == "" {
			errs = append(errs, errors.New("invalid namespaces: entries must not be empty"))
			continue
		}
		entries = append(entries, ns)
	}

	if len(entries) == 0 && len(errs) == 0 {
		return []error{errors.New(`no namespaces configured: set "namespaces: [<namespace>]", or "namespaces: [\"*\"]" to discover Services cluster-wide`)}
	}

	hasAll := false
	others := 0

	for _, ns := range entries {
		if ns == AllNamespaces {
			hasAll = true
			continue
		}
		others++
		if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(msgs, "; ")))
		}
	}

	if hasAll && others > 0 {
		errs = append(errs, fmt.Errorf("invalid namespaces: %q must be the only entry, it cannot be combined with other namespaces", AllNamespaces))
	}

	return errs

}

// parseListenerAddress splits and validates a listenerAddress, returning its
// host (possibly empty) and port.
func parseListenerAddress(addr string) (string, int, error) {

	if addr == "" {
		return "", 0, errors.New("'listenerAddress' cannot be empty")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid listenerAddress %q: %w (write the port as \":8080\" or \"<ip>:8080\")", addr, err)
	}

	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid listenerAddress %q: port %q must be a number between 1 and 65535", addr, portStr)
	}

	if host != "" && net.ParseIP(host) == nil {
		return "", 0, fmt.Errorf("invalid listenerAddress %q: host %q must be empty or an IP address", addr, host)
	}

	return host, int(port), nil

}

// isWildcardHost reports whether a listener host binds every address.
func isWildcardHost(host string) bool {

	if host == "" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsUnspecified()

}

// listenersConflict reports whether two listener addresses cannot be bound
// together: same port and the same host, or either host is a wildcard.
func listenersConflict(hostA string, portA int, hostB string, portB int) bool {

	if portA != portB {
		return false
	}

	if isWildcardHost(hostA) || isWildcardHost(hostB) {
		return true
	}

	return net.ParseIP(hostA).Equal(net.ParseIP(hostB))

}

// Validate validates the whole configuration, running every configuration's
// checks plus the cross-configuration ones, and reports all problems at once.
func (c *Config) Validate() error {

	if len(c.BackendConfigurations) == 0 {
		return errors.New("no configurations defined: 'configurations' must contain at least one entry")
	}

	var errs []error

	prefix := func(i int) string {
		return fmt.Sprintf("configurations[%d] (%s)", i, c.BackendConfigurations[i].Name)
	}

	names := make(map[string]int)

	type listener struct {
		host string
		port int
	}
	listeners := make(map[int]listener)

	for i := range c.BackendConfigurations {

		bc := &c.BackendConfigurations[i]

		for _, err := range bc.problems() {
			errs = append(errs, fmt.Errorf("%s: %w", prefix(i), err))
		}

		if bc.Name != "" {
			if first, ok := names[bc.Name]; ok {
				errs = append(errs, fmt.Errorf("%s: duplicate name, already used by configurations[%d]", prefix(i), first))
			} else {
				names[bc.Name] = i
			}
		}

		host, port, err := parseListenerAddress(bc.ListenerAddress)
		if err != nil {
			continue
		}

		for j := 0; j < i; j++ {

			other := listeners[j]
			if other.port == 0 {
				continue
			}

			if listenersConflict(host, port, other.host, other.port) {
				errs = append(errs, fmt.Errorf("%s: listenerAddress %q conflicts with configurations[%d] (%s) listenerAddress %q",
					prefix(i), bc.ListenerAddress, j, c.BackendConfigurations[j].Name, c.BackendConfigurations[j].ListenerAddress))
				break
			}

		}

		listeners[i] = listener{host: host, port: port}

	}

	return errors.Join(errs...)

}

// GetListenerPort extracts the port number from ListenerAddress
func (bc *Configuration) GetListenerPort() (int, error) {

	_, port, err := net.SplitHostPort(strings.TrimSpace(bc.ListenerAddress))
	if err != nil {
		return 0, fmt.Errorf("invalid listenerAddress '%s': %w", bc.ListenerAddress, err)
	}

	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, fmt.Errorf("invalid listenerAddress '%s': %w", bc.ListenerAddress, err)
	}

	return n, nil

}
