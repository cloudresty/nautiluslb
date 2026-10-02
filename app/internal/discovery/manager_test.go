package discovery

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

type call struct {
	pool string
	eps  []backend.Endpoint
}

type recSink struct {
	mu    sync.Mutex
	calls []call
}

func (s *recSink) SetEndpoints(pool string, eps []backend.Endpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call{pool, slices.Clone(eps)})
}

func (s *recSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// last returns the addresses of the most recent call for pool, and whether
// there was one.
func (s *recSink) last(pool string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].pool == pool {
			return addrStrings(s.calls[i].eps), true
		}
	}
	return nil, false
}

func (s *recSink) callsFor(pool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.pool == pool {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitEndpoints(t *testing.T, s *recSink, pool string, want ...string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("pool %s = %v", pool, want), func() bool {
		got, ok := s.last(pool)
		return ok && slices.Equal(got, want)
	})
}

// watchCounter counts established watches so a test can create objects only
// once the informers are really watching (the fake clientset loses events
// between a list and its watch).
func watchCounter(client *fake.Clientset) *atomic.Int32 {
	var n atomic.Int32
	client.PrependWatchReactor("*", func(k8stesting.Action) (bool, watch.Interface, error) {
		n.Add(1)
		return false, nil, nil
	})
	return &n
}

func waitWatches(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	waitFor(t, "watches", func() bool { return n.Load() >= want })
	time.Sleep(30 * time.Millisecond)
}

func testOpts(sink Sink, debounce time.Duration) Options {
	return Options{
		Settings: config.DiscoverySettings{ResyncPeriod: config.Duration(time.Hour), Debounce: config.Duration(debounce), Nodes: v4},
		Sink:     sink,
	}
}

func startManager(t *testing.T, client *fake.Clientset, opts Options, specs ...config.PoolSpec) *Manager {
	t.Helper()
	m := NewManager(client, opts)
	if err := m.Start(context.Background(), specs); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Stop)
	return m
}

func TestInformerUpdateReachesSink(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"))
	watches := watchCounter(client)
	sink := &recSink{}
	spec := poolSpec("https", "ingress")
	startManager(t, client, testOpts(sink, 10*time.Millisecond), spec)
	waitWatches(t, watches, 3)

	svc := nodePortSvc("ingress", "ingress", bound("https"), corev1.ServicePort{Name: "https", NodePort: 30742})
	if _, err := client.CoreV1().Services("ingress").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEndpoints(t, sink, "https", "10.0.0.1:30742")

	// A weight change is an endpoint change.
	svc = svc.DeepCopy()
	svc.Annotations[weightKey] = "5"
	if _, err := client.CoreV1().Services("ingress").Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "weight 5", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		c := sink.calls[len(sink.calls)-1]
		return len(c.eps) == 1 && c.eps[0].Weight == 5
	})

	// Removing the Service empties the (synced) pool.
	if err := client.CoreV1().Services("ingress").Delete(context.Background(), "ingress", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "empty pool", func() bool {
		got, ok := sink.last("https")
		return ok && len(got) == 0
	})
}

func TestStartAppliesInitialState(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("ingress", "ingress", bound("https"), corev1.ServicePort{Name: "https", NodePort: 30742}))
	sink := &recSink{}
	startManager(t, client, testOpts(sink, 10*time.Millisecond), poolSpec("https", "ingress"))
	// Start returns only after the first reconcile.
	if got, ok := sink.last("https"); !ok || !slices.Equal(got, []string{"10.0.0.1:30742"}) {
		t.Fatalf("after Start: %v %v", got, ok)
	}
}

func TestDebounceCoalesces(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"))
	watches := watchCounter(client)
	sink := &recSink{}
	startManager(t, client, testOpts(sink, 300*time.Millisecond), poolSpec("https", "ns"))
	waitWatches(t, watches, 3)
	base := sink.count()

	for i := range 10 {
		svc := nodePortSvc("ns", fmt.Sprintf("s%d", i), bound("https"), corev1.ServicePort{Name: "https", NodePort: int32(30000 + i)})
		if _, err := client.CoreV1().Services("ns").Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "10 endpoints", func() bool {
		got, _ := sink.last("https")
		return len(got) == 10
	})
	time.Sleep(700 * time.Millisecond) // no straggler reconcile
	if n := sink.count() - base; n < 1 || n > 2 {
		t.Fatalf("10 rapid updates caused %d sink calls, want 1-2", n)
	}
}

// block makes list calls for resource (in namespace ns, "*" = any) fail until
// the returned release function is called; the reflector keeps retrying, so the
// informer stays unsynced meanwhile. (A reactor that merely waited would hold
// the fake clientset's lock and stall every other call.)
func block(client *fake.Clientset, resource, ns string) (release func()) {
	var released atomic.Bool
	client.PrependReactor("list", resource, func(a k8stesting.Action) (bool, kruntime.Object, error) {
		if !released.Load() && (ns == "*" || a.GetNamespace() == ns) {
			return true, nil, errors.New("list unavailable")
		}
		return false, nil, nil
	})
	return func() { released.Store(true) }
}

type syncRecorder struct {
	metrics.Recorder
	mu     sync.Mutex
	synced map[string]bool
	skips  int
}

func (r *syncRecorder) InformerSynced(resource string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synced[resource] = ok
}

func (r *syncRecorder) DiscoveryReconcile(result string, _ time.Duration) {
	if result == "skipped" {
		r.mu.Lock()
		r.skips++
		r.mu.Unlock()
	}
}

func TestUnsyncedStoreNeverWipes(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30001}))
	watches := watchCounter(client)
	sink := &recSink{}
	rec := &syncRecorder{Recorder: metrics.NewNop(), synced: map[string]bool{}}
	opts := testOpts(sink, 10*time.Millisecond)
	opts.Recorder = rec
	m := startManager(t, client, opts, poolSpec("p", "a"))
	waitWatches(t, watches, 3)
	waitEndpoints(t, sink, "p", "10.0.0.1:30001")

	// The only Service goes away, and the pool is widened to a namespace whose
	// list hangs: the pool must keep its endpoints, not be emptied.
	release := block(client, "services", "b")
	defer release()
	if err := client.CoreV1().Services("a").Delete(context.Background(), "svc", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a", "b")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "skipped reconcile", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.skips > 0 && !rec.synced["services"]
	})
	time.Sleep(100 * time.Millisecond)
	if got, _ := sink.last("p"); !slices.Equal(got, []string{"10.0.0.1:30001"}) {
		t.Fatalf("pool wiped while a store was unsynced: %v (calls %d)", got, sink.callsFor("p"))
	}

	// Once the store syncs the empty result is real and is applied.
	release()
	waitFor(t, "empty after sync", func() bool {
		got, _ := sink.last("p")
		return len(got) == 0
	})
}

func TestUnsyncedNodesSkipOnlyNodePools(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "np", bound("np"), corev1.ServicePort{Name: "https", NodePort: 30001}),
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "cip", Namespace: "a", Annotations: bound("cip")},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.1",
				Ports: []corev1.ServicePort{{Name: "https", Port: 443}}},
		})
	release := block(client, "nodes", "*")
	defer release()
	sink := &recSink{}
	m := NewManager(client, testOpts(sink, 10*time.Millisecond))
	m.syncTimeout = 300 * time.Millisecond
	err := m.Start(context.Background(), []config.PoolSpec{poolSpec("np", "a"), poolSpec("cip", "a")})
	t.Cleanup(m.Stop)
	if !errors.Is(err, ErrNotSynced) {
		t.Fatalf("Start = %v, want ErrNotSynced", err)
	}
	waitEndpoints(t, sink, "cip", "10.96.0.1:443")
	if n := sink.callsFor("np"); n != 0 {
		t.Fatalf("node pool applied %d times without nodes", n)
	}
	release()
	waitEndpoints(t, sink, "np", "10.0.0.1:30001")
}

func TestScopedConfigNeedsNoClusterList(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30001}),
		nodePortSvc("other", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30002}))
	var violations atomic.Int32
	forbid := func(a k8stesting.Action) (bool, kruntime.Object, error) {
		if a.GetNamespace() == metav1.NamespaceAll {
			violations.Add(1)
			return true, nil, errors.New("forbidden: cluster-wide access")
		}
		return false, nil, nil
	}
	for _, res := range []string{"services", "endpointslices"} {
		client.PrependReactor("list", res, forbid)
		client.PrependWatchReactor(res, func(a k8stesting.Action) (bool, watch.Interface, error) {
			ok, _, err := forbid(a)
			return ok, nil, err
		})
	}
	sink := &recSink{}
	startManager(t, client, testOpts(sink, 10*time.Millisecond), poolSpec("p", "a"))
	waitEndpoints(t, sink, "p", "10.0.0.1:30001")
	if n := violations.Load(); n != 0 {
		t.Fatalf("%d cluster-wide list/watch calls for scoped config", n)
	}
}

func TestScopedSpecDoesNotReadClusterWideFactory(t *testing.T) {
	// Both a cluster-wide and a scoped pool exist; the scoped one must only
	// ever see its own namespace.
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "svc", bound("wide", "scoped"), corev1.ServicePort{Name: "https", NodePort: 30001}),
		nodePortSvc("b", "svc", bound("wide", "scoped"), corev1.ServicePort{Name: "https", NodePort: 30002}))
	sink := &recSink{}
	startManager(t, client, testOpts(sink, 10*time.Millisecond), poolSpec("wide", ""), poolSpec("scoped", "a"))
	waitEndpoints(t, sink, "wide", "10.0.0.1:30001", "10.0.0.1:30002")
	waitEndpoints(t, sink, "scoped", "10.0.0.1:30001")
}

func TestRebindAddsNamespace(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30001}),
		nodePortSvc("b", "svc", bound("p", "q"), corev1.ServicePort{Name: "https", NodePort: 30002}))
	sink := &recSink{}
	m := startManager(t, client, testOpts(sink, 10*time.Millisecond), poolSpec("p", "a"))
	waitEndpoints(t, sink, "p", "10.0.0.1:30001")

	if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a", "b"), poolSpec("q", "b")}); err != nil {
		t.Fatal(err)
	}
	waitEndpoints(t, sink, "p", "10.0.0.1:30001", "10.0.0.1:30002")
	waitEndpoints(t, sink, "q", "10.0.0.1:30002")

	// Dropping namespace a stops its factory and its Services.
	if err := m.Rebind([]config.PoolSpec{poolSpec("p", "b")}); err != nil {
		t.Fatal(err)
	}
	waitEndpoints(t, sink, "p", "10.0.0.1:30002")
	m.mu.Lock()
	var namespaces []string
	for ns := range m.facs {
		namespaces = append(namespaces, ns)
	}
	m.mu.Unlock()
	if !slices.Equal(namespaces, []string{"b"}) {
		t.Fatalf("factories = %v, want only b", namespaces)
	}
}

func TestRebindFailureKeepsPreviousState(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30001}),
		nodePortSvc("b", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30002}))
	sink := &recSink{}
	m := startManager(t, client, testOpts(sink, 10*time.Millisecond), poolSpec("p", "a"))
	waitEndpoints(t, sink, "p", "10.0.0.1:30001")

	m.mu.Lock()
	m.factoryHook = func(ns string) (*nsFactory, error) {
		if ns == "c" {
			return nil, errors.New("boom")
		}
		return m.newNSFactory(ns)
	}
	m.mu.Unlock()
	if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a", "b", "c")}); err == nil {
		t.Fatal("Rebind succeeded, want error")
	}
	m.mu.Lock()
	var namespaces []string
	for ns := range m.facs {
		namespaces = append(namespaces, ns)
	}
	specs := slices.Clone(m.specs)
	m.mu.Unlock()
	if !slices.Equal(namespaces, []string{"a"}) {
		t.Fatalf("factories = %v, want only a", namespaces)
	}
	if len(specs) != 1 || !slices.Equal(specs[0].Namespaces, []string{"a"}) {
		t.Fatalf("specs changed: %+v", specs)
	}
	m.signal()
	time.Sleep(100 * time.Millisecond)
	waitEndpoints(t, sink, "p", "10.0.0.1:30001")
}

func TestStopNoGoroutineLeak(t *testing.T) {
	run := func() {
		client := fake.NewClientset(node("n1", "10.0.0.1"),
			nodePortSvc("a", "svc", bound("p"), corev1.ServicePort{Name: "https", NodePort: 30001}))
		m := NewManager(client, testOpts(&recSink{}, 10*time.Millisecond))
		if err := m.Start(context.Background(), []config.PoolSpec{poolSpec("p", "a", "b")}); err != nil {
			t.Fatal(err)
		}
		if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a")}); err != nil {
			t.Fatal(err)
		}
		if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a", "c")}); err != nil {
			t.Fatal(err)
		}
		m.Stop()
		m.Stop() // idempotent
	}
	run() // absorb one-time process goroutines (klog flusher etc.)
	before := runtime.NumGoroutine()
	for range 3 {
		run()
	}
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(5 * time.Second); n > before && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	if n > before {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutines: %d after Stop, %d before\n%s", n, before, buf)
	}
}

func TestRebindRemoveReaddResendsEndpoints(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"),
		nodePortSvc("a", "svc", bound("p", "x"), corev1.ServicePort{Name: "https", NodePort: 30001}))
	sink := &recSink{}
	m := startManager(t, client, testOpts(sink, 300*time.Millisecond), poolSpec("p", "a"), poolSpec("x", "a"))
	waitEndpoints(t, sink, "x", "10.0.0.1:30001")
	before := sink.callsFor("x")

	// Remove and re-add x faster than the debounce: no reconcile sees the gap.
	if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a")}); err != nil {
		t.Fatal(err)
	}
	if err := m.Rebind([]config.PoolSpec{poolSpec("p", "a"), poolSpec("x", "a")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "x endpoints resent", func() bool { return sink.callsFor("x") > before })
}

func TestSyncedClosesOnce(t *testing.T) {
	client := fake.NewClientset(node("n1", "10.0.0.1"))
	m := startManager(t, client, testOpts(&recSink{}, 10*time.Millisecond), poolSpec("p", "a"))
	select {
	case <-m.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("Synced not closed after Start returned nil")
	}
}
