package kube

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cloudresty/nautiluslb/internal/config"
)

func writeKubeconfig(t *testing.T, dir, name, server, ctx string) string {
	t.Helper()
	body := "apiVersion: v1\nkind: Config\ncurrent-context: " + ctx + "\n" +
		"clusters:\n- name: c\n  cluster:\n    server: " + server + "\n" +
		"contexts:\n- name: " + ctx + "\n  context:\n    cluster: c\n    user: u\n" +
		"users:\n- name: u\n  user:\n    token: t\n"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKubeconfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	explicit := writeKubeconfig(t, dir, "explicit", "https://explicit.example:6443", "ctx-explicit")
	env1 := writeKubeconfig(t, dir, "env1", "https://env1.example:6443", "ctx-env1")
	env2 := writeKubeconfig(t, dir, "env2", "https://env2.example:6443", "ctx-env2")
	home := writeKubeconfig(t, dir, "home", "https://home.example:6443", "ctx-home")

	oldHome := clientcmd.RecommendedHomeFile
	clientcmd.RecommendedHomeFile = home
	t.Cleanup(func() { clientcmd.RecommendedHomeFile = oldHome })

	oldIn := inCluster
	t.Cleanup(func() { inCluster = oldIn })
	inCluster = func() (*rest.Config, error) { return nil, rest.ErrNotInCluster }

	tests := []struct {
		name     string
		explicit string
		env      string
		wantHost string
		wantCtx  string
	}{
		{"explicit beats KUBECONFIG", explicit, env1, "https://explicit.example:6443", "ctx-explicit"},
		{"KUBECONFIG beats home", "", env1, "https://env1.example:6443", "ctx-env1"},
		{"KUBECONFIG with several paths uses the first current-context", "", env1 + string(os.PathListSeparator) + env2, "https://env1.example:6443", "ctx-env1"},
		{"home as last resort", "", "", "https://home.example:6443", "ctx-home"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tt.env)
			cfg, ctx, err := restConfig(config.KubernetesSettings{Kubeconfig: tt.explicit, QPS: 7, Burst: 9}, "")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Host != tt.wantHost || ctx != tt.wantCtx {
				t.Fatalf("host=%q ctx=%q, want %q %q", cfg.Host, ctx, tt.wantHost, tt.wantCtx)
			}
			if cfg.QPS != 7 || cfg.Burst != 9 || cfg.UserAgent == "" {
				t.Fatalf("qps=%v burst=%d ua=%q", cfg.QPS, cfg.Burst, cfg.UserAgent)
			}
			if _, _, err := NewClient(config.KubernetesSettings{Kubeconfig: tt.explicit}, "ua/1"); err != nil {
				t.Fatalf("NewClient: %v", err)
			}
		})
	}

	t.Run("in-cluster wins over everything", func(t *testing.T) {
		inCluster = func() (*rest.Config, error) { return &rest.Config{Host: "https://in-cluster"}, nil }
		defer func() { inCluster = func() (*rest.Config, error) { return nil, rest.ErrNotInCluster } }()
		t.Setenv("KUBECONFIG", env1)
		cfg, ctx, err := restConfig(config.KubernetesSettings{Kubeconfig: explicit}, "custom/1")
		if err != nil || cfg.Host != "https://in-cluster" || ctx != "in-cluster" || cfg.UserAgent != "custom/1" {
			t.Fatalf("cfg=%+v ctx=%q err=%v", cfg, ctx, err)
		}
	})

	t.Run("missing explicit file is an error", func(t *testing.T) {
		t.Setenv("KUBECONFIG", env1)
		_, _, err := NewClient(config.KubernetesSettings{Kubeconfig: filepath.Join(dir, "nope")}, "")
		if err == nil {
			t.Fatal("expected error")
		}
		var pe *os.PathError
		if !errors.As(err, &pe) {
			t.Logf("error (not a PathError, still wrapped): %v", err)
		}
	})
}
