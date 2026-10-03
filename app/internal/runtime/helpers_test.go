package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// log is an ordered, goroutine-safe event log shared by recorder and hooks.
type log struct {
	mu sync.Mutex
	ev []string
}

func (l *log) add(s string) {
	l.mu.Lock()
	l.ev = append(l.ev, s)
	l.mu.Unlock()
}

func (l *log) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func (l *log) index(s string) int {
	for i, e := range l.snapshot() {
		if e == s {
			return i
		}
	}
	return -1
}

type fakeRec struct {
	metrics.Recorder
	l *log
}

func (f fakeRec) Ready(ok bool)              { f.l.add(fmt.Sprintf("Ready(%v)", ok)) }
func (f fakeRec) ConfigReload(result string) { f.l.add("ConfigReload(" + result + ")") }
func (f fakeRec) ConnRejected(l, reason string) {
	f.l.add("ConnRejected(" + l + "," + reason + ")")
}

// freeAddr reserves-then-releases a port. Only for tests that need an address
// string known before (or distinct from) the bind: a reload that moves a
// listener to a new address, or a check that a port was released.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	_ = ln.Close()
	return a
}

// echoBackend is a loopback TCP echo server; it returns its port.
func echoBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func udpEchoBackend(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteTo(buf[:n], from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

const header = `apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
settings:
  drain: {readinessDelay: 10ms, timeout: %s}
  discovery: {debounce: 10ms}
%sconfigurations:
`

// doc renders a configuration document from tcpCfg/udpCfg blocks.
func doc(timeout string, blocks ...string) string {
	return docWith(timeout, "", blocks...)
}

func docWith(timeout, settingsExtra string, blocks ...string) string {
	return fmt.Sprintf(header, timeout, settingsExtra) + strings.Join(blocks, "")
}

func tcpCfg(name, addr, extra string) string {
	return fmt.Sprintf("  - name: %s\n    listenerAddress: %q\n    namespaces: [default]\n    backendPortName: http\n    health: {type: none}\n%s", name, addr, extra)
}

func udpCfg(name, addr string) string {
	return fmt.Sprintf("  - name: %s\n    protocol: udp\n    listenerAddress: %q\n    namespaces: [default]\n    backendPortName: dns\n    health: {type: none}\n", name, addr)
}

func mustCfg(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse("test.yaml", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	// Validate rejects port 0, so anyAddr entries are swapped for unique
	// placeholders while validating and restored afterwards.
	var wild []int
	for i := range cfg.Configurations {
		if cfg.Configurations[i].ListenerAddress == anyAddr {
			wild = append(wild, i)
			cfg.Configurations[i].ListenerAddress = fmt.Sprintf("127.0.0.1:%d", 1+i)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, i := range wild {
		cfg.Configurations[i].ListenerAddress = anyAddr
	}
	return cfg
}

// svc is a ClusterIP Service at 127.0.0.1 whose port is the backend port.
func svc(name string, cfgs string, port int, proto corev1.Protocol, portName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{
			config.ServiceEnabledAnnotation:        "true",
			config.ServiceConfigurationsAnnotation: cfgs,
		}},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "127.0.0.1",
			Ports: []corev1.ServicePort{{Name: portName, Port: int32(port), Protocol: proto}},
		},
	}
}

func tcpSvc(name, cfgs string, port int) *corev1.Service {
	return svc(name, cfgs, port, corev1.ProtocolTCP, "http")
}

type harness struct {
	rt  *Runtime
	log *log
	cs  *fake.Clientset
}

// newRT builds a Runtime (New only, not started).
func newRT(t *testing.T, yaml string, mod func(*Options), objs ...kruntime.Object) *harness {
	t.Helper()
	h := &harness{log: &log{}, cs: fake.NewSimpleClientset(objs...)}
	opts := Options{
		Config: mustCfg(t, yaml), Client: h.cs, Recorder: fakeRec{Recorder: metrics.NewNop(), l: h.log},
		trace: h.log.add,
	}
	if mod != nil {
		mod(&opts)
	}
	rt, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.rt = rt
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt.Shutdown(ctx)
	})
	return h
}

// startRT builds and starts a Runtime and waits until it is ready.
func startRT(t *testing.T, yaml string, mod func(*Options), objs ...kruntime.Object) *harness {
	t.Helper()
	h := newRT(t, yaml, mod, objs...)
	if err := h.rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, why := h.rt.Ready(); !ok {
		t.Fatalf("not ready after Start: %s", why)
	}
	return h
}

// anyAddr asks the kernel for a free port; the bound address is read back
// with harness.addr.
const anyAddr = "127.0.0.1:0"

func (h *harness) addr(name string) string {
	a, ok := h.rt.ListenerAddr(name)
	if !ok {
		h.rt.opts.trace("no listener " + name)
	}
	return a
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// echo writes msg and reports whether it came back.
func echo(c net.Conn, msg string) bool {
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return false
	}
	buf := make([]byte, len(msg))
	_, err := io.ReadFull(c, buf)
	return err == nil && string(buf) == msg
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitProxied dials addr until a full echo round trip works (endpoints arrived).
func waitProxied(t *testing.T, addr string) net.Conn {
	t.Helper()
	var got net.Conn
	eventually(t, "proxied echo on "+addr, func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		if echo(c, "ping") {
			got = c
			return true
		}
		_ = c.Close()
		return false
	})
	t.Cleanup(func() { _ = got.Close() })
	return got
}

// refused reports whether a new connection to addr gets no echo.
func refused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	defer func() { _ = c.Close() }()
	return !echo(c, "x")
}

func block(t *testing.T, cs *fake.Clientset, resource string) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, kruntime.Object, error) {
		<-gate
		return false, nil, nil
	})
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}
