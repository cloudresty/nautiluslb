package health

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

type fakeRec struct {
	metrics.Recorder
	mu     sync.Mutex
	ok     int
	fail   int
	health []bool
}

func newFakeRec() *fakeRec { return &fakeRec{Recorder: metrics.NewNop()} }

func (r *fakeRec) ProbeDone(_, _, result string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if result == "ok" {
		r.ok++
	} else {
		r.fail++
	}
}

func (r *fakeRec) BackendHealth(_, _, _ string, healthy bool, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health = append(r.health, healthy)
}

func (r *fakeRec) healthEvents() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.health...)
}

func (r *fakeRec) probes() (ok, fail int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ok, r.fail
}

type funcProber func(ctx context.Context, addr string) error

func (f funcProber) Probe(ctx context.Context, addr string) error { return f(ctx, addr) }

func newBackend(port uint16) *backend.Backend {
	return backend.New(backend.Endpoint{Address: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), Weight: 1}, 0)
}

func fastOpts(p Prober, rec metrics.Recorder) Options {
	return Options{
		Interval: 2 * time.Millisecond, Timeout: time.Second,
		Rise: 2, Fall: 3, Prober: p, Recorder: rec,
		Listener: "l", Pool: "p",
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRiseFall(t *testing.T) {

	var failing atomic.Bool
	var calls atomic.Int64
	p := funcProber(func(context.Context, string) error {
		calls.Add(1)
		if failing.Load() {
			return errors.New("down")
		}
		return nil
	})
	rec := newFakeRec()
	c := NewChecker(fastOpts(p, rec))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newBackend(1)
	c.Start(ctx)
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()

	// Healthy stays healthy while probes succeed.
	eventually(t, "some probes", func() bool { return calls.Load() >= 5 })
	if !b.IsHealthy() {
		t.Fatal("backend should stay healthy")
	}

	failing.Store(true)
	eventually(t, "unhealthy event", func() bool { return len(rec.healthEvents()) >= 1 })

	failing.Store(false)
	eventually(t, "healthy event", func() bool { return len(rec.healthEvents()) >= 2 })

	ev := rec.healthEvents()
	if len(ev) != 2 || ev[0] || !ev[1] {
		t.Fatalf("health transitions = %v, want [false true]", ev)
	}

}

func TestRiseFallThresholds(t *testing.T) {

	// Alternating results never reach Fall=3 consecutive failures.
	var n atomic.Int64
	p := funcProber(func(context.Context, string) error {
		if n.Add(1)%2 == 0 {
			return errors.New("flap")
		}
		return nil
	})
	rec := newFakeRec()
	c := NewChecker(fastOpts(p, rec))
	b := newBackend(1)
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	eventually(t, "probes", func() bool { return n.Load() >= 30 })
	c.Stop()
	if !b.IsHealthy() {
		t.Fatal("flapping probe must not eject")
	}

}

func TestJitterWithinBounds(t *testing.T) {

	c := NewChecker(Options{Interval: 10 * time.Second, Timeout: time.Second, Jitter: 0.2})
	o := c.opts.Load()
	lo, hi := 8*time.Second, 12*time.Second

	for _, r := range []float64{0, 0.25, 0.5, 0.999999, 1} {
		c.rnd = func() float64 { return r }
		d := c.nextDelay(o)
		if d < lo || d > hi {
			t.Fatalf("rnd=%v delay %v outside [%v,%v]", r, d, lo, hi)
		}
	}

	c.rnd = func() float64 { return 0.5 }
	if d := c.nextDelay(o); d != 10*time.Second {
		t.Fatalf("midpoint delay = %v", d)
	}

	for range 1000 {
		c2 := NewChecker(Options{Interval: 10 * time.Second, Timeout: time.Second, Jitter: 0.2})
		if d := c2.nextDelay(c2.opts.Load()); d < lo || d > hi {
			t.Fatalf("real rand delay %v out of bounds", d)
		}
	}

}

func TestFirstProbeDephased(t *testing.T) {

	delays := make(chan time.Duration, 8)
	c := NewChecker(Options{
		Interval: 10 * time.Second, Timeout: time.Second, Rise: 1, Fall: 1,
		Prober: funcProber(func(context.Context, string) error { return nil }),
	})
	c.rnd = func() float64 { return 0.3 }
	c.sleep = func(ctx context.Context, d time.Duration) bool {
		select {
		case delays <- d:
		default:
		}
		<-ctx.Done()
		return false
	}
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{newBackend(1)})
	defer c.Stop()

	select {
	case d := <-delays:
		if d != 3*time.Second {
			t.Fatalf("first delay = %v, want 3s (0.3*interval)", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no first delay observed")
	}

	// Real rand: first delay always within [0, interval).
	for range 1000 {
		c2 := NewChecker(Options{Interval: time.Second, Timeout: time.Second})
		if d := c2.firstDelay(c2.opts.Load()); d < 0 || d >= time.Second {
			t.Fatalf("first delay %v outside [0, interval)", d)
		}
	}

}

func TestEjectionHoldIgnoresEarlySuccess(t *testing.T) {

	var calls atomic.Int64
	p := funcProber(func(context.Context, string) error { calls.Add(1); return nil })
	rec := newFakeRec()
	c := NewChecker(fastOpts(p, rec))
	b := newBackend(1)
	hold := 300 * time.Millisecond
	b.SetEjectionHold(hold)
	b.SetHealthy(false, backend.CausePassive)
	until := b.EjectedUntil()
	if !until.After(time.Now()) {
		t.Skip("backend did not record an ejection hold")
	}

	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()

	eventually(t, "many successes", func() bool { return calls.Load() >= 10 })
	if time.Now().Before(until) && b.IsHealthy() {
		t.Fatal("backend recovered before ejection hold elapsed")
	}

	eventually(t, "recovery after hold", func() bool { return b.IsHealthy() })
	if time.Now().Before(until) {
		t.Fatal("recovered before hold elapsed")
	}

}

func TestReconcileKeepsRunningLoops(t *testing.T) {

	var mu sync.Mutex
	seen := map[string]int{}
	p := funcProber(func(_ context.Context, addr string) error {
		mu.Lock()
		seen[addr]++
		mu.Unlock()
		return nil
	})
	count := func(addr string) int { mu.Lock(); defer mu.Unlock(); return seen[addr] }

	c := NewChecker(fastOpts(p, newFakeRec()))
	c.Start(context.Background())
	defer c.Stop()

	a, b := newBackend(1), newBackend(2)
	c.Reconcile([]*backend.Backend{a, b})

	c.mu.Lock()
	la, lb := c.loops[a.Address()], c.loops[b.Address()]
	c.mu.Unlock()
	if la == nil || lb == nil {
		t.Fatal("loops not started for both backends")
	}

	// Same objects again: loops untouched (not restarted).
	c.Reconcile([]*backend.Backend{a, b})
	c.mu.Lock()
	if c.loops[a.Address()] != la || c.loops[b.Address()] != lb || len(c.loops) != 2 {
		c.mu.Unlock()
		t.Fatal("running loops were restarted or duplicated")
	}
	c.mu.Unlock()

	// Remove b, add d: b cancelled, a kept, d started.
	d := newBackend(3)
	c.Reconcile([]*backend.Backend{a, d})
	c.mu.Lock()
	if c.loops[a.Address()] != la {
		c.mu.Unlock()
		t.Fatal("loop for kept backend restarted")
	}
	if _, ok := c.loops[b.Address()]; ok {
		c.mu.Unlock()
		t.Fatal("loop for removed backend still registered")
	}
	if c.loops[d.Address()] == nil {
		c.mu.Unlock()
		t.Fatal("loop for new backend not started")
	}
	c.mu.Unlock()

	eventually(t, "new backend probed", func() bool { return count(d.Address()) > 0 })
	// Removed loop must stop probing.
	time.Sleep(20 * time.Millisecond)
	before := count(b.Address())
	time.Sleep(30 * time.Millisecond)
	if after := count(b.Address()); after != before {
		t.Fatalf("removed backend still probed (%d -> %d)", before, after)
	}

	// Empty set cancels everything.
	c.Reconcile(nil)
	c.mu.Lock()
	n := len(c.loops)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("loops after empty reconcile = %d", n)
	}

}

func TestReconcileBeforeStart(t *testing.T) {

	var n atomic.Int64
	p := funcProber(func(context.Context, string) error { n.Add(1); return nil })
	c := NewChecker(fastOpts(p, newFakeRec()))
	c.Reconcile([]*backend.Backend{newBackend(1)})
	time.Sleep(20 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatal("probed before Start")
	}
	c.Start(context.Background())
	c.Start(context.Background()) // idempotent
	defer c.Stop()
	eventually(t, "probe after Start", func() bool { return n.Load() > 0 })

}

func TestStopWaits(t *testing.T) {

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	p := funcProber(func(ctx context.Context, _ string) error {
		<-ctx.Done() // blocks until cancelled
		return ctx.Err()
	})
	c := NewChecker(fastOpts(p, newFakeRec()))
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{newBackend(1), newBackend(2), newBackend(3)})
	time.Sleep(30 * time.Millisecond)

	done := make(chan struct{})
	go func() { c.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}

	eventually(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= base })

}

func TestCancelledProbeCountsNothing(t *testing.T) {

	started := make(chan struct{}, 1)
	p := funcProber(func(ctx context.Context, _ string) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	})
	rec := newFakeRec()
	o := fastOpts(p, rec)
	o.Fall = 1
	c := NewChecker(o)
	b := newBackend(1)
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe never started")
	}
	c.Stop()

	if !b.IsHealthy() {
		t.Fatal("cancelled probe must not eject a backend")
	}
	if ok, fail := rec.probes(); ok != 0 || fail != 0 {
		t.Fatalf("cancelled probe recorded ok=%d fail=%d", ok, fail)
	}

}

func TestProbeTimeoutCountsAsFailure(t *testing.T) {

	p := funcProber(func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() })
	o := fastOpts(p, newFakeRec())
	o.Timeout, o.Fall = 5*time.Millisecond, 2
	c := NewChecker(o)
	b := newBackend(1)
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()
	eventually(t, "timeouts eject", func() bool { return !b.IsHealthy() })

}

func TestUpdateAppliesNextTick(t *testing.T) {

	var failing atomic.Bool
	failing.Store(true)
	p := funcProber(func(context.Context, string) error {
		if failing.Load() {
			return errors.New("down")
		}
		return nil
	})
	o := fastOpts(p, newFakeRec())
	o.Fall = 1000 // effectively never ejects
	c := NewChecker(o)
	b := newBackend(1)
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()

	time.Sleep(30 * time.Millisecond)
	if !b.IsHealthy() {
		t.Fatal("ejected despite Fall=1000")
	}

	o.Fall = 1
	c.Update(o)
	eventually(t, "new Fall applied", func() bool { return !b.IsHealthy() })

}

func TestNoneProberReportsOnly(t *testing.T) {

	rec := newFakeRec()
	c := NewChecker(Options{Interval: time.Millisecond, Timeout: time.Millisecond, Recorder: rec, Listener: "l", Pool: "p"})
	c.Start(context.Background())
	defer c.Stop()

	b := newBackend(1)
	c.Reconcile([]*backend.Backend{b})
	c.Reconcile([]*backend.Backend{b})

	c.mu.Lock()
	n := len(c.loops)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("none prober started %d loops, want 1 (passive recovery only)", n)
	}
	if ev := rec.healthEvents(); len(ev) != 0 {
		t.Fatalf("health reports = %v, want none without a transition", ev)
	}
	if ok, fail := rec.probes(); ok+fail != 0 {
		t.Fatal("none prober must not probe")
	}

}

func TestNoneProberRecoversAfterEjectionHold(t *testing.T) {

	rec := newFakeRec()
	o := fastOpts(nil, rec)
	c := NewChecker(o)
	b := newBackend(1)
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()

	const hold = 50 * time.Millisecond
	start := time.Now()
	b.SetEjectionHold(hold)
	b.SetHealthy(false, backend.CausePassive)

	eventually(t, "recovery", func() bool { return b.IsHealthy() })
	if el := time.Since(start); el < hold {
		t.Fatalf("recovered after %v, before the %v hold", el, hold)
	}
	eventually(t, "event", func() bool { return len(rec.healthEvents()) == 1 })
	if ev := rec.healthEvents(); !ev[0] {
		t.Fatalf("events = %v, want [true]", ev)
	}

}

func TestNoneProberReconcileNoTransitions(t *testing.T) {

	rec := newFakeRec()
	c := NewChecker(fastOpts(nil, rec))
	b := newBackend(1)
	c.Start(context.Background())
	for range 5 {
		c.Reconcile([]*backend.Backend{b})
	}
	time.Sleep(20 * time.Millisecond)
	c.Stop()

	if ev := rec.healthEvents(); len(ev) != 0 {
		t.Fatalf("transitions = %v, want none", ev)
	}

}

type transitionLog struct {
	mu sync.Mutex
	ev []string
}

func (l *transitionLog) hook(_ *backend.Backend, healthy bool, cause backend.Cause) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := "down"
	if healthy {
		s = "up"
	}
	if cause == backend.CausePassive {
		s += "/passive"
	} else {
		s += "/probe"
	}
	l.ev = append(l.ev, s)
}

func (l *transitionLog) events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func TestOnTransitionProbeUpDown(t *testing.T) {
	var fail atomic.Bool
	p := funcProber(func(context.Context, string) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	})
	var log transitionLog
	o := fastOpts(p, newFakeRec())
	o.OnTransition = log.hook
	c := NewChecker(o)
	b := newBackend(1)
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()

	fail.Store(true)
	eventually(t, "down", func() bool { return len(log.events()) == 1 })
	fail.Store(false)
	eventually(t, "up", func() bool { return len(log.events()) == 2 })
	if got := log.events(); got[0] != "down/probe" || got[1] != "up/probe" {
		t.Fatalf("events %v", got)
	}
}

func TestOnTransitionPassiveRecovery(t *testing.T) {
	var log transitionLog
	o := fastOpts(nil, newFakeRec())
	o.OnTransition = log.hook
	c := NewChecker(o)
	b := newBackend(1)
	if !c.Eject(b, 20*time.Millisecond) {
		t.Fatal("Eject reported no change")
	}
	c.Start(context.Background())
	c.Reconcile([]*backend.Backend{b})
	defer c.Stop()
	eventually(t, "passive recovery", func() bool { return len(log.events()) == 2 })
	if got := log.events(); got[0] != "down/passive" || got[1] != "up/passive" {
		t.Fatalf("events %v", got)
	}
}

func TestEjectIdempotentAndRecorded(t *testing.T) {
	var log transitionLog
	rec := newFakeRec()
	o := fastOpts(nil, rec)
	o.OnTransition = log.hook
	c := NewChecker(o)
	b := newBackend(1)
	if !c.Eject(b, time.Minute) || b.IsHealthy() {
		t.Fatal("first Eject must flip to unhealthy")
	}
	if c.Eject(b, time.Minute) {
		t.Fatal("second Eject must report no change")
	}
	if !b.EjectedUntil().After(time.Now()) {
		t.Fatal("hold not set")
	}
	if got := log.events(); len(got) != 1 || got[0] != "down/passive" {
		t.Fatalf("events %v", got)
	}
	if h := rec.healthEvents(); len(h) != 1 || h[0] {
		t.Fatalf("metrics %v", h)
	}
}
