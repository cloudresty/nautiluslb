//go:build integration

// Package integration runs the real Runtime end to end, in process, against a
// fake Kubernetes clientset and real loopback backends. Run it with
// `make integration` (-tags integration).
package integration
