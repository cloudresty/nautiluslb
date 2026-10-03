package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

func ep(port int, weight int) backend.Endpoint {
	return backend.Endpoint{Address: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)), Weight: weight}
}

func eps(ports ...int) []backend.Endpoint {
	out := make([]backend.Endpoint, 0, len(ports))
	for _, p := range ports {
		out = append(out, ep(p, 1))
	}
	return out
}

func spec() config.PoolSpec {
	return config.PoolSpec{
		Key:      "web",
		Balancer: config.Balancer{Algorithm: "round_robin"},
		Health: config.Health{
			Type: config.HealthNone, Interval: config.Duration(20 * time.Millisecond),
			Timeout: config.Duration(100 * time.Millisecond), EjectionHold: config.Duration(time.Minute),
			Rise: 1, Fall: 1,
		},
	}
}

func newPool(t testing.TB, s config.PoolSpec, rec metrics.Recorder) *Pool {
	t.Helper()
	p, err := New(Options{Spec: s, Listener: "l", Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestNewUnknownAlgorithm(t *testing.T) {
	s := spec()
	s.Balancer.Algorithm = "nope"
	if _, err := New(Options{Spec: s}); !errors.Is(err, balancer.ErrUnknownAlgorithm) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetEndpointsPreservesBackendObjects(t *testing.T) {
	p := newPool(t, spec(), nil)
	p.SetEndpoints(eps(1, 2, 3))
	before := map[string]*backend.Backend{}
	for _, b := range p.Snapshot().All {
		before[b.Address()] = b
	}
	b2 := before["127.0.0.1:2"]
	b2.SetHealthy(false, backend.CauseProbe)
	if !b2.TryAcquire() {
		t.Fatal("acquire")
	}

	p.SetEndpoints(eps(2, 3, 4))
	after := map[string]*backend.Backend{}
	for _, b := range p.Snapshot().All {
		after[b.Address()] = b
	}
	if len(after) != 3 || after["127.0.0.1:2"] != b2 || after["127.0.0.1:3"] != before["127.0.0.1:3"] {
		t.Fatal("backend objects not preserved")
	}
	if after["127.0.0.1:2"].IsHealthy() || b2.ActiveConnections() != 1 {
		t.Fatal("health/counters lost")
	}
	if after["127.0.0.1:4"] == nil {
		t.Fatal("new backend missing")
	}

	// weight change replaces the object
	p.SetEndpoints([]backend.Endpoint{ep(2, 5), ep(3, 1)})
	for _, b := range p.Snapshot().All {
		if b.Address() == "127.0.0.1:2" && (b == b2 || b.Endpoint().Weight != 5) {
			t.Fatal("weight change not applied")
		}
		if b.Address() == "127.0.0.1:3" && b != before["127.0.0.1:3"] {
			t.Fatal("unchanged backend replaced")
		}
	}
}

func TestPickFailsOpen(t *testing.T) {
	p := newPool(t, spec(), nil)
	p.SetEndpoints(eps(1, 2))
	for _, b := range p.Snapshot().All {
		p.Eject(b)
	}
	if len(p.Snapshot().Healthy) != 0 {
		t.Fatal("expected none healthy")
	}
	for i := 0; i < 10; i++ {
		if got := p.Pick(balancer.Key{}, 2); len(got) != 2 {
			t.Fatalf("fail-open pick = %d", len(got))
		}
	}
	if !p.open.Load() {
		t.Fatal("open flag not set")
	}
	// warn-once: flag stays set across picks; re-armed once healthy exists
	p.Snapshot().All[0].SetHealthy(true, backend.CauseProbe)
	p.refresh()
	if p.open.Load() {
		t.Fatal("flag not reset")
	}
	if got := p.Pick(balancer.Key{}, 2); len(got) != 1 || got[0] != p.Snapshot().All[0] {
		t.Fatalf("expected only recovered backend, got %d", len(got))
	}
	p.Eject(p.Snapshot().All[0])
	p.Pick(balancer.Key{}, 1)
	if !p.open.Load() {
		t.Fatal("second transition did not re-enter fail-open")
	}
}

func TestPickEmptyPool(t *testing.T) {
	p := newPool(t, spec(), nil)
	if got := p.Pick(balancer.Key{}, 2); len(got) != 0 {
		t.Fatal("empty pool must pick nothing")
	}
}

func TestEjectStopsPicksImmediately(t *testing.T) {
	p := newPool(t, spec(), nil)
	p.SetEndpoints(eps(1, 2, 3))
	victim := p.Snapshot().All[1]
	p.Eject(victim)
	if victim.EjectedUntil().IsZero() {
		t.Fatal("hold not set")
	}
	for i := 0; i < 100; i++ {
		for _, b := range p.Pick(balancer.Key{}, 3) {
			if b == victim {
				t.Fatal("ejected backend picked")
			}
		}
	}
}

func listenLoop(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l, l.Addr().(*net.TCPAddr).Port
}

func TestEjectThenRecoverViaChecker(t *testing.T) {
	l, port := listenLoop(t)
	defer func() { _ = l.Close() }()
	s := spec()
	s.Health.Type = config.HealthTCP
	s.Health.EjectionHold = config.Duration(60 * time.Millisecond)
	p := newPool(t, s, nil)
	p.SetEndpoints(eps(port))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	b := p.Snapshot().All[0]
	p.Eject(b)
	if got := p.Pick(balancer.Key{}, 1); len(got) != 1 { // fail-open
		t.Fatal("fail-open expected")
	}
	if len(p.Snapshot().Healthy) != 0 {
		t.Fatal("must be ejected")
	}
	eventually(t, func() bool { return len(p.Snapshot().Healthy) == 1 })
	if !b.IsHealthy() {
		t.Fatal("not recovered")
	}
}

func TestCheckerMarksDownAndHealthPortOverride(t *testing.T) {
	l, hport := listenLoop(t)
	defer func() { _ = l.Close() }()
	s := spec()
	s.Health.Type = config.HealthTCP
	s.Health.Port = hport
	p := newPool(t, s, nil)
	p.SetEndpoints(eps(1)) // traffic port 1 is dead, health port is alive
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()
	time.Sleep(150 * time.Millisecond)
	if len(p.Snapshot().Healthy) != 1 {
		t.Fatal("health.port override not used")
	}
	_ = l.Close()
	eventually(t, func() bool { return len(p.Snapshot().Healthy) == 0 })
}

func TestUpdateKeepsEndpoints(t *testing.T) {
	p := newPool(t, spec(), nil)
	p.SetEndpoints(eps(1, 2))
	before := p.Snapshot().All
	s := spec()
	s.Balancer.Algorithm = "least_conn"
	s.MaxConnectionsPerBackend = 7
	if err := p.Update(s); err != nil {
		t.Fatal(err)
	}
	after := p.Snapshot().All
	if len(after) != 2 || after[0] != before[0] || after[1] != before[1] {
		t.Fatal("endpoints changed by Update")
	}
	if len(p.Pick(balancer.Key{}, 1)) != 1 {
		t.Fatal("pick after update")
	}
	// cap applies to backends created from now on, and replaces on next SetEndpoints
	p.SetEndpoints(eps(1, 2, 3))
	for _, b := range p.Snapshot().All {
		if b.MaxConnections() != 7 {
			t.Fatalf("cap = %d", b.MaxConnections())
		}
	}
	s.Balancer.Algorithm = "bogus"
	if err := p.Update(s); !errors.Is(err, balancer.ErrUnknownAlgorithm) {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateNoneToTCPStartsProbes(t *testing.T) {
	l, port := listenLoop(t)
	_ = l.Close() // dead port: a probe must mark it down
	p := newPool(t, spec(), nil)
	p.SetEndpoints(eps(port))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()
	time.Sleep(80 * time.Millisecond)
	if len(p.Snapshot().Healthy) != 1 {
		t.Fatal("none type must not mark down")
	}
	s := spec()
	s.Health.Type = config.HealthTCP
	if err := p.Update(s); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(p.Snapshot().Healthy) == 0 })
}

func TestPickNeverMutatesSnapshot(t *testing.T) {
	p := newPool(t, spec(), nil)
	p.SetEndpoints(eps(1, 2, 3, 4))
	snap := p.Snapshot()
	all := append([]*backend.Backend(nil), snap.All...)
	healthy := append([]*backend.Backend(nil), snap.Healthy...)
	for i := 0; i < 50; i++ {
		got := p.Pick(balancer.Key{SourceIP: netip.MustParseAddr("10.0.0.1")}, 3)
		_ = got
	}
	for i := range all {
		if snap.All[i] != all[i] || snap.Healthy[i] != healthy[i] {
			t.Fatal("snapshot mutated")
		}
	}
	if p.Snapshot() != snap {
		t.Fatal("snapshot replaced without change")
	}
}

type sizeRec struct {
	metrics.Recorder
	mu          sync.Mutex
	healthy, tt int
	pool        string
	cause       []string
}

func (r *sizeRec) PoolSize(l, pool string, h, total int) {
	r.mu.Lock()
	r.healthy, r.tt, r.pool = h, total, pool
	r.mu.Unlock()
}
func (r *sizeRec) BackendHealth(l, pool, addr string, healthy bool, cause string) {
	r.mu.Lock()
	r.cause = append(r.cause, cause)
	r.mu.Unlock()
}
func (r *sizeRec) get() (int, int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.healthy, r.tt, r.pool
}

func TestPoolSizeMetric(t *testing.T) {
	r := &sizeRec{Recorder: metrics.NewNop()}
	p := newPool(t, spec(), r)
	p.SetEndpoints(eps(1, 2, 3))
	if h, n, k := r.get(); h != 3 || n != 3 || k != "web" {
		t.Fatalf("got %d/%d %s", h, n, k)
	}
	p.Eject(p.Snapshot().All[0])
	if h, n, _ := r.get(); h != 2 || n != 3 {
		t.Fatalf("got %d/%d", h, n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cause) != 1 || r.cause[0] != "passive" {
		t.Fatalf("causes %v", r.cause)
	}
}

func TestStopWaits(t *testing.T) {
	before := runtime.NumGoroutine()
	l, port := listenLoop(t)
	s := spec()
	s.Health.Type = config.HealthTCP
	p := newPool(t, s, nil)
	p.SetEndpoints(eps(port, port+1, port+2))
	p.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	_ = l.Close()
	eventually(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

func BenchmarkPick(b *testing.B) {
	p := newPool(b, spec(), nil)
	p.SetEndpoints(eps(1, 2, 3))
	key := balancer.Key{SourceIP: netip.MustParseAddr("10.0.0.1")}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if len(p.Pick(key, 3)) == 0 {
				b.Fatal("empty")
			}
		}
	})
}

func BenchmarkPickUnderChurn(b *testing.B) {
	p := newPool(b, spec(), nil)
	p.SetEndpoints(eps(1, 2, 3))
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			p.SetEndpoints(eps(1, 2, 3, 4+i%4))
			time.Sleep(50 * time.Microsecond)
		}
	}()
	key := balancer.Key{SourceIP: netip.MustParseAddr("10.0.0.1")}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if len(p.Pick(key, 3)) == 0 {
				b.Fatal("empty")
			}
		}
	})
	b.StopTimer()
	stop.Store(true)
	wg.Wait()
}

func pickedHas(p *Pool, b *backend.Backend) bool {
	for _, x := range p.Pick(balancer.Key{}, 3) {
		if x == b {
			return true
		}
	}
	return false
}

func TestEveryTransitionRefreshesPool(t *testing.T) {
	la, pa := listenLoop(t)
	defer func() { _ = la.Close() }()
	lb, pb := listenLoop(t)
	s := spec()
	s.Health.Type = config.HealthTCP
	s.Health.Rise, s.Health.Fall = 1, 1
	s.Health.EjectionHold = config.Duration(time.Millisecond)
	p := newPool(t, s, nil) // plain Nop recorder: nothing rides on metrics
	p.SetEndpoints(eps(pa, pb))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()
	a, b := p.Snapshot().All[0], p.Snapshot().All[1]

	// probe down
	_ = lb.Close()
	eventually(t, func() bool { return !b.IsHealthy() })
	eventually(t, func() bool { return !pickedHas(p, b) && len(p.Snapshot().Healthy) == 1 })

	// probe up
	lb2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", pb))
	if err != nil {
		t.Skipf("cannot re-listen: %v", err)
	}
	defer func() { _ = lb2.Close() }()
	go func() {
		for {
			c, err := lb2.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	eventually(t, func() bool { return pickedHas(p, b) && len(p.Snapshot().Healthy) == 2 })

	// Eject: reflected immediately, no waiting
	s2 := spec()
	s2.Health.EjectionHold = config.Duration(time.Hour)
	if err := p.Update(s2); err != nil {
		t.Fatal(err)
	}
	p.Eject(a)
	if pickedHas(p, a) || len(p.Snapshot().Healthy) != 1 {
		t.Fatal("Eject not reflected synchronously")
	}
}

func TestPassiveRecoveryRefreshesPool(t *testing.T) {
	s := spec() // health none: recovery is passive after the hold
	s.Health.EjectionHold = config.Duration(40 * time.Millisecond)
	p := newPool(t, s, nil)
	p.SetEndpoints(eps(1, 2))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()
	a := p.Snapshot().All[0]
	p.Eject(a)
	if pickedHas(p, a) {
		t.Fatal("ejected backend still picked")
	}
	eventually(t, func() bool { return len(p.Snapshot().Healthy) == 2 && pickedHas(p, a) })
}
