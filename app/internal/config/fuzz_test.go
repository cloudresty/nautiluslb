package config

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzParse(f *testing.F) {
	f.Add([]byte(validDoc))
	f.Add([]byte(""))
	f.Add([]byte("---\n"))
	f.Add([]byte(validDoc + "---\n" + validDoc))
	f.Add([]byte("configurations: [1, 2"))
	f.Add([]byte("configurations:\n  - name: a\n    namespces: x\n"))
	seeds := []string{
		filepath.Join("..", "..", "config.example.yaml"),
		filepath.Join("..", "..", "config.yaml"),
	}
	if m, err := filepath.Glob(filepath.Join("testdata", "*.yaml")); err == nil {
		seeds = append(seeds, m...)
	}
	for _, p := range seeds {
		if data, err := os.ReadFile(p); err == nil {
			f.Add(data)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse("fuzz.yaml", data)
		if err != nil {
			return
		}
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err != nil {
			return
		}
		// Anything accepted must be fully usable by discovery and the proxies
		for _, bc := range cfg.Configurations {
			if len(bc.DiscoveryNamespaces()) == 0 {
				t.Fatalf("accepted configuration %q with no discovery namespaces", bc.Name)
			}
			if _, err := bc.GetListenerPort(); err != nil {
				t.Fatalf("accepted configuration %q with bad listener: %v", bc.Name, err)
			}
			pools := bc.Pools()
			if len(pools) == 0 {
				t.Fatalf("accepted configuration %q with no pools", bc.Name)
			}
			for _, p := range pools {
				if p.BackendPortName == "" || p.Key == "" {
					t.Fatalf("accepted pool %+v without port name or key", p)
				}
			}
			if d := bc.EffectiveDialTimeout(); d <= 0 || d > MaxDialTimeout {
				t.Fatalf("dial timeout %v out of range", d)
			}
		}
	})
}
