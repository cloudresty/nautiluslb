package config

import (
	"sort"
	"strings"
)

// ParseBinding splits a Service annotation entry: "a/b" -> ("a","b"), "a" -> ("a","").
func ParseBinding(entry string) (config, route string) {
	config, route, _ = strings.Cut(strings.TrimSpace(entry), "/")
	return strings.TrimSpace(config), strings.TrimSpace(route)
}

// DiscoveryNamespaces returns the namespaces to list Services in: Namespace
// and Namespaces merged, deduplicated and sorted. Cluster-wide discovery is
// returned as a single metav1.NamespaceAll (""). Call it only on a validated
// configuration.
func (c *Configuration) DiscoveryNamespaces() []string {
	seen := make(map[string]bool)
	var namespaces []string
	for _, ns := range append([]string{c.Namespace}, c.Namespaces...) {
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
