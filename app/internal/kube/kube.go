// Package kube builds the Kubernetes client used by discovery.
package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/version"
)

// inCluster is replaced in tests: rest.InClusterConfig reads the environment
// and the service account mount.
var inCluster = rest.InClusterConfig

// NewClient builds a client and reports the context it talks to ("in-cluster"
// or the kubeconfig's current context). Precedence: in-cluster config, then
// s.Kubeconfig, then $KUBECONFIG (possibly several paths), then ~/.kube/config.
// QPS, Burst and UserAgent come from s; an empty userAgent defaults to
// "nautiluslb/<version>".
func NewClient(s config.KubernetesSettings, userAgent string) (kubernetes.Interface, string, error) {
	cfg, kubeContext, err := restConfig(s, userAgent)
	if err != nil {
		return nil, "", err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("creating Kubernetes client: %w", err)
	}
	return client, kubeContext, nil
}

func restConfig(s config.KubernetesSettings, userAgent string) (*rest.Config, string, error) {
	cfg, err := inCluster()
	kubeContext := "in-cluster"
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if s.Kubeconfig != "" {
			rules.ExplicitPath = s.Kubeconfig
		}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
		cfg, err = loader.ClientConfig()
		if err != nil {
			return nil, "", fmt.Errorf("loading Kubernetes config (kubeconfig %q): %w", s.Kubeconfig, err)
		}
		raw, err := loader.RawConfig()
		if err != nil {
			return nil, "", fmt.Errorf("reading kubeconfig: %w", err)
		}
		kubeContext = raw.CurrentContext
	}

	if s.QPS > 0 {
		cfg.QPS = s.QPS
	}
	if s.Burst > 0 {
		cfg.Burst = s.Burst
	}
	if userAgent == "" {
		userAgent = s.UserAgent
	}
	if userAgent == "" {
		userAgent = "nautiluslb/" + version.Version
	}
	cfg.UserAgent = userAgent
	return cfg, kubeContext, nil
}
