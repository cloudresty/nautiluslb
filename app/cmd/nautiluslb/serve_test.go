package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cloudresty/nautiluslb/internal/config"
	nlbruntime "github.com/cloudresty/nautiluslb/internal/runtime"
)

const testHeader = "apiVersion: nautiluslb.cloudresty.io/v2\nkind: Config\n"

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(testHeader+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func serveCfg(t *testing.T, settings string) string {
	return writeCfg(t, "settings:\n  drain: {readinessDelay: 10ms, timeout: 1s}\n  discovery: {debounce: 10ms}\n"+settings+
		"configurations:\n  - name: web\n    listenerAddress: \""+freeAddr(t)+"\"\n    backendPortName: http\n    namespaces: [default]\n")
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func useFakeClient(t *testing.T, err error) {
	t.Helper()
	prev := newKubeClient
	newKubeClient = func(config.KubernetesSettings, string) (kubernetes.Interface, string, error) {
		if err != nil {
			return nil, "", err
		}
		return fake.NewClientset(), "fake", nil
	}
	t.Cleanup(func() { newKubeClient = prev })
}

func TestHelp(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--help"}, noEnv, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Usage:", "Options:", "Configuration:", "Admin endpoints", "Signals:", "--validate", "SIGHUP"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestFlagParseOutcomes(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"-h"}, noEnv, &out); code != 0 {
		t.Fatalf("-h exit %d", code)
	}
	if code := run([]string{"--nope"}, noEnv, &out); code != 2 {
		t.Fatalf("unknown flag exit %d, want 2", code)
	}
}

func TestConfigFromEnvAndSummary(t *testing.T) {
	p := writeCfg(t, `settings:
  kubeconfigPath: "/x"
configurations:
  - name: legacy
    listenerAddress: ":8080"
    requestTimeout: 3
    backendPortName: http
    namespace: default
`)
	t.Setenv("NLB_KUBECONFIG", "")
	_ = os.Unsetenv("NLB_KUBECONFIG")
	var out bytes.Buffer
	env := func(k string) string {
		if k == "NLB_CONFIG" {
			return p
		}
		return ""
	}
	if code := run([]string{"--validate"}, env, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, want := range []string{"is valid: 1 configuration(s)", "legacy: protocol=", "namespaces=default", "deprecation: settings.kubeconfigPath", "deprecation: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
}

func TestMissingConfigExits1(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--config", filepath.Join(t.TempDir(), "none.yaml")}, noEnv, &out); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if code := run([]string{"--validate", "--config", filepath.Join(t.TempDir(), "none.yaml")}, noEnv, &out); code != 1 || !strings.Contains(out.String(), "invalid:") {
		t.Fatalf("exit %d out %q", code, out.String())
	}
}

func TestServeGracefulSigterm(t *testing.T) {
	useFakeClient(t, nil)
	p := serveCfg(t, "  admin: {address: \""+freeAddr(t)+"\"}\n  accessLog: {enabled: true, output: stderr}\n")
	code := make(chan int, 1)
	go func() { code <- run([]string{"--config", p, "--pprof"}, noEnv, &bytes.Buffer{}) }()
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("exit %d, want 0", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit on SIGTERM")
	}
}

func TestServeStartupFailures(t *testing.T) {
	t.Run("kube client", func(t *testing.T) {
		useFakeClient(t, errors.New("no cluster"))
		if code := run([]string{"--config", serveCfg(t, "  admin: {address: \"\"}\n")}, noEnv, &bytes.Buffer{}); code != 1 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("access log", func(t *testing.T) {
		useFakeClient(t, nil)
		p := serveCfg(t, "  accessLog: {enabled: true, output: /nonexistent-dir/x/access.log}\n")
		if code := run([]string{"--config", p}, noEnv, &bytes.Buffer{}); code != 1 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("bind", func(t *testing.T) {
		useFakeClient(t, nil)
		p := writeCfg(t, "settings:\n  admin: {address: \"\"}\nconfigurations:\n  - name: web\n    listenerAddress: \"256.0.0.1:1\"\n    backendPortName: http\n    namespaces: [default]\n")
		if code := run([]string{"--config", p}, noEnv, &bytes.Buffer{}); code != 1 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("admin", func(t *testing.T) {
		useFakeClient(t, nil)
		p := serveCfg(t, "  admin: {address: \"256.0.0.1:1\"}\n")
		if code := run([]string{"--config", p}, noEnv, &bytes.Buffer{}); code != 1 {
			t.Fatalf("exit %d", code)
		}
	})
}

func TestCloseResourcesOrder(t *testing.T) {
	var order []string
	closeResources(
		func(context.Context) error { order = append(order, "log"); return errors.New("log boom") },
		func(context.Context) error { order = append(order, "admin"); return errors.New("admin boom") },
	)
	if strings.Join(order, ",") != "log,admin" {
		t.Fatalf("order %v", order)
	}
}

// recRuntime records reloads and can fail Start.
type recRuntime struct {
	startErr   error
	reloads    chan *config.Config
	failReload atomic.Bool
	shutdown   chan struct{}
}

func (r *recRuntime) Start(context.Context) error { return r.startErr }
func (r *recRuntime) Reload(c *config.Config) (nlbruntime.Summary, error) {
	r.reloads <- c
	var err error
	if r.failReload.Load() {
		err = errors.New("rejected by runtime")
	}
	return nlbruntime.Summary{Added: []string{"a"}, Ignored: []string{"settings.x"}}, err
}
func (r *recRuntime) Shutdown(context.Context) nlbruntime.Summary {
	select {
	case r.shutdown <- struct{}{}:
	default:
	}
	return nlbruntime.Summary{}
}

func TestLifecycleStartError(t *testing.T) {
	rt := &recRuntime{startErr: errors.New("boom"), shutdown: make(chan struct{}, 1)}
	if code := lifecycle(rt, nopRec{}, &config.Config{}, "x", make(chan os.Signal, 1), func() {}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if len(rt.shutdown) != 1 {
		t.Fatal("runtime not shut down after failed Start")
	}
}

func TestLifecycleStartCanceledShutsDown(t *testing.T) {
	rt := &recRuntime{startErr: context.Canceled, shutdown: make(chan struct{}, 1)}
	closed := false
	if code := lifecycle(rt, nopRec{}, &config.Config{}, "x", make(chan os.Signal, 1), func() { closed = true }); code != 0 || !closed {
		t.Fatalf("exit %d closed %v", code, closed)
	}
}

type countRec struct{ got chan string }

func (c countRec) ConfigReload(r string) { c.got <- r }

func TestLifecycleReloadPaths(t *testing.T) {
	p := serveCfg(t, "  reload: {watchFile: true}\n")
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	rt := &recRuntime{reloads: make(chan *config.Config, 4), shutdown: make(chan struct{}, 1)}
	rec := countRec{got: make(chan string, 4)}
	sigs := make(chan os.Signal, 4)
	code := make(chan int, 1)
	go func() { code <- lifecycle(rt, rec, cfg, p, sigs, func() {}) }()

	sigs <- syscall.SIGHUP // successful reload with ignored settings
	select {
	case <-rt.reloads:
	case <-time.After(3 * time.Second):
		t.Fatal("no reload")
	}

	rt.failReload.Store(true)
	sigs <- syscall.SIGHUP
	select {
	case <-rt.reloads:
	case <-time.After(3 * time.Second):
		t.Fatal("no second reload")
	}

	if err := os.WriteFile(p, []byte("not: [valid"), 0o600); err != nil { // unloadable file
		t.Fatal(err)
	}
	sigs <- syscall.SIGHUP
	select {
	case r := <-rec.got:
		if r != "rejected" {
			t.Fatalf("recorded %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bad file reload not rejected")
	}

	sigs <- syscall.SIGTERM
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("exit %d", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle did not return")
	}
}

func TestWatchFileRemovedAndRecreated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() { watchFile(ctx, p, 5*time.Millisecond, out); close(done) }()
	time.Sleep(30 * time.Millisecond)
	_ = os.Remove(p)
	time.Sleep(30 * time.Millisecond)
	select {
	case <-out:
		t.Fatal("removal must not fire an event")
	default:
	}
	if err := os.WriteFile(p, []byte("recreated, longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("recreation not detected")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchFile did not stop")
	}
}
