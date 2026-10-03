//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
	nlb "github.com/cloudresty/nautiluslb/internal/runtime"
)

// ---- configuration builders ------------------------------------------------

const healthNone = "    health: {type: none}\n"

// doc renders a configuration document. drain is settings.drain.timeout.
func doc(drain string, blocks ...string) string {
	return fmt.Sprintf(`apiVersion: nautiluslb.cloudresty.io/v1
kind: Config
settings:
  drain: {readinessDelay: 10ms, timeout: %s}
  discovery: {debounce: 10ms}
configurations:
`, drain) + strings.Join(blocks, "")
}

// tcpConf renders a tcp configuration; extra holds whole lines indented by 4.
func tcpConf(name, addr, extra string) string {
	return fmt.Sprintf("  - name: %s\n    listenerAddress: %q\n    namespaces: [default]\n    backendPortName: http\n%s", name, addr, extra)
}

func udpConf(name, addr string) string {
	return fmt.Sprintf("  - name: %s\n    protocol: udp\n    listenerAddress: %q\n    namespaces: [default]\n    backendPortName: dns\n%s", name, addr, healthNone)
}

func mustCfg(t testing.TB, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse("integration.yaml", []byte(yaml))
	if err != nil {
		t.Fatalf("parsing config: %v\n%s", err, yaml)
	}
	cfg.ApplyDefaults()
	// Validate rejects port 0: swap anyAddr for unique placeholders while
	// validating and restore it afterwards.
	var wild []int
	for i := range cfg.Configurations {
		if cfg.Configurations[i].ListenerAddress == anyAddr {
			wild = append(wild, i)
			cfg.Configurations[i].ListenerAddress = fmt.Sprintf("127.0.0.1:%d", 1+i)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating config: %v\n%s", err, yaml)
	}
	for _, i := range wild {
		cfg.Configurations[i].ListenerAddress = anyAddr
	}
	return cfg
}

// anyAddr asks the kernel for a free port at bind time; read the bound address
// back with env.addr. mustCfg lets it through Validate, which rejects port 0.
const anyAddr = "127.0.0.1:0"

// ---- Services ---------------------------------------------------------------

// svc is a ClusterIP Service at 127.0.0.1 whose port is the backend port, so
// discovery computes the loopback endpoint 127.0.0.1:port.
func svc(name, bindings string, port int, proto corev1.Protocol, portName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{
			config.ServiceEnabledAnnotation:        "true",
			config.ServiceConfigurationsAnnotation: bindings,
		}},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "127.0.0.1",
			Ports: []corev1.ServicePort{{Name: portName, Port: int32(port), Protocol: proto}},
		},
	}
}

// tcpSvc binds backend b (port name "http") to the comma-separated bindings.
func tcpSvc(b *tcpBackend, bindings string) *corev1.Service {
	return svc("svc-"+b.id, bindings, b.port, corev1.ProtocolTCP, "http")
}

// ---- access log sink --------------------------------------------------------

type alogSink struct {
	mu   sync.Mutex
	recs []accesslog.Record
}

func (s *alogSink) Log(r accesslog.Record) {
	s.mu.Lock()
	s.recs = append(s.recs, r)
	s.mu.Unlock()
}

func (s *alogSink) count(pred func(accesslog.Record) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.recs {
		if pred(r) {
			n++
		}
	}
	return n
}

func (s *alogSink) find(pred func(accesslog.Record) bool) (accesslog.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if pred(r) {
			return r, true
		}
	}
	return accesslog.Record{}, false
}

// ---- environment --------------------------------------------------------------

type env struct {
	t   *testing.T
	rt  *nlb.Runtime
	cs  *fake.Clientset
	reg *prometheus.Registry
	log *alogSink
}

// startEnv builds and starts a Runtime over a fake clientset holding objs and
// waits for readiness. The Runtime is shut down (forced) when the test ends.
func startEnv(t *testing.T, yaml string, mod func(*nlb.Options), objs ...kruntime.Object) *env {
	t.Helper()
	e := &env{t: t, cs: fake.NewSimpleClientset(objs...), reg: metrics.NewRegistry(), log: &alogSink{}}
	metrics.RegisterBuildInfo(e.reg)
	opts := nlb.Options{
		Config:    mustCfg(t, yaml),
		Client:    e.cs,
		Recorder:  metrics.NewPrometheus(e.reg, true),
		AccessLog: e.log,
	}
	if mod != nil {
		mod(&opts)
	}
	rt, err := nlb.New(opts)
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	e.rt = rt
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rt.Shutdown(ctx)
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	if ok, why := rt.Ready(); !ok {
		t.Fatalf("not ready after Start: %s", why)
	}
	return e
}

// addr is the bound address of the named configuration's listener.
func (e *env) addr(name string) string {
	e.t.Helper()
	a, ok := e.rt.ListenerAddr(name)
	if !ok {
		e.t.Fatalf("no listener %q", name)
	}
	return a
}

func (e *env) setService(s *corev1.Service) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.cs.CoreV1().Services(s.Namespace).Update(ctx, s, metav1.UpdateOptions{}); err == nil {
		return
	}
	if _, err := e.cs.CoreV1().Services(s.Namespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
		e.t.Fatalf("creating service %s: %v", s.Name, err)
	}
}

func (e *env) deleteService(name string) {
	e.t.Helper()
	if err := e.cs.CoreV1().Services("default").Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		e.t.Fatalf("deleting service %s: %v", name, err)
	}
}

func (e *env) reload(yaml string) (nlb.Summary, error) {
	e.t.Helper()
	return e.rt.Reload(mustCfg(e.t, yaml))
}

// ---- metrics -------------------------------------------------------------------

// metric sums the samples of the named family whose labels include every
// k,v pair given (counter and gauge values; histogram sample counts).
func (e *env) metric(name string, kv ...string) float64 {
	e.t.Helper()
	fams, err := e.reg.Gather()
	if err != nil {
		e.t.Fatalf("gathering metrics: %v", err)
	}
	var sum float64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			lbls := map[string]string{}
			for _, l := range m.GetLabel() {
				lbls[l.GetName()] = l.GetValue()
			}
			for i := 0; i+1 < len(kv); i += 2 {
				if lbls[kv[i]] != kv[i+1] {
					continue next
				}
			}
			switch {
			case m.Counter != nil:
				sum += m.GetCounter().GetValue()
			case m.Gauge != nil:
				sum += m.GetGauge().GetValue()
			case m.Histogram != nil:
				sum += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return sum
}

const mp = "nautiluslb_"

// waitPool waits until the pool has exactly n healthy backends.
func (e *env) waitPool(listener, pool string, n int) {
	e.t.Helper()
	eventually(e.t, fmt.Sprintf("pool %s to have %d healthy backends", pool, n), func() bool {
		return int(e.metric(mp+"pool_backends", "listener", listener, "pool", pool, "state", "healthy")) == n
	})
}

func (e *env) waitMetric(what string, min float64, name string, kv ...string) {
	e.t.Helper()
	eventually(e.t, what, func() bool { return e.metric(name, kv...) >= min })
}

// ---- polling and clients ---------------------------------------------------------

// eventually polls cond every 5ms until it holds or 10s pass.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// client is a connection to the load balancer with a line reader.
type client struct {
	net.Conn
	br *bufio.Reader
}

func newClient(c net.Conn) *client { return &client{Conn: c, br: bufio.NewReader(c)} }

// dialLB connects to addr; the connection is closed at test end.
func dialLB(t testing.TB, addr string) *client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return newClient(c)
}

// line reads one line (without the newline) within 3s.
func (c *client) line() (string, error) {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	s, err := c.br.ReadString('\n')
	return strings.TrimSuffix(s, "\n"), err
}

// id reads the backend's greeting line.
func (c *client) id() (string, error) { return c.line() }

// echo sends msg and reports whether it came back unchanged.
func (c *client) echo(msg string) bool {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return false
	}
	buf := make([]byte, len(msg))
	_, err := io.ReadFull(c.br, buf)
	return err == nil && string(buf) == msg
}

// connectID dials the balancer and returns the greeting of the backend that
// served the connection. The connection is returned open.
func connectID(t testing.TB, addr string) (*client, string) {
	t.Helper()
	c := dialLB(t, addr)
	id, err := c.id()
	if err != nil {
		t.Fatalf("no backend greeting from %s: %v", addr, err)
	}
	return c, id
}

// rejected reports whether a new connection to addr is closed without a greeting.
func rejected(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 1))
	return n == 0 && err != nil
}

func httpGet(t testing.TB, url string) (int, string) {
	t.Helper()
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type accesslogRecord = accesslog.Record
