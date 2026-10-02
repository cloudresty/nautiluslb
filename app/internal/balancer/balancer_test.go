package balancer

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

func mk(i, weight, max int) *backend.Backend {
	ap := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, byte(i / 250), byte(i % 250)}), 8080)
	return backend.New(backend.Endpoint{Address: ap, Weight: weight}, max)
}

func pool(weights ...int) []*backend.Backend {
	out := make([]*backend.Backend, len(weights))
	for i, w := range weights {
		out[i] = mk(i+1, w, 0)
	}
	return out
}

func mustNew(t testing.TB, alg string, o Options) Picker {
	t.Helper()
	p, err := New(alg, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ipKey(i int) Key {
	return Key{SourceIP: netip.AddrFrom4([4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})}
}

func TestUnknownAlgorithm(t *testing.T) {
	if _, err := New("nope", Options{}); !errors.Is(err, ErrUnknownAlgorithm) {
		t.Fatal(err)
	}
}

func TestRoundRobinWeighted(t *testing.T) {
	bs := pool(1, 2, 7)
	snap := backend.NewSnapshot(bs)
	p := mustNew(t, "round_robin", Options{})
	p.Rebuild(snap)
	const N = 100000
	got := map[*backend.Backend]int{}
	for i := 0; i < N; i++ {
		got[p.Pick(snap, Key{}, 1)[0]]++
	}
	for i, w := range []int{1, 2, 7} {
		want := float64(w) / 10
		if d := math.Abs(float64(got[bs[i]])/N - want); d > 0.01 {
			t.Errorf("backend %d share %.4f want %.2f", i, float64(got[bs[i]])/N, want)
		}
	}
}

func TestLeastConn(t *testing.T) {
	bs := pool(1, 1, 2)
	snap := backend.NewSnapshot(bs)
	p := mustNew(t, "least_conn", Options{})
	for i := 0; i < 5; i++ {
		bs[0].TryAcquire()
	}
	for i := 0; i < 3; i++ {
		bs[1].TryAcquire()
	}
	for i := 0; i < 4; i++ {
		// Load ends at 4/2 = 2.0, between 3 and 5.
		bs[2].TryAcquire()
	}
	got := p.Pick(snap, Key{}, 3)
	if got[0] != bs[2] || got[1] != bs[1] || got[2] != bs[0] {
		t.Fatalf("order wrong: %v", addrs(got))
	}
	// ties rotate
	eq := pool(1, 1, 1)
	es := backend.NewSnapshot(eq)
	seen := map[*backend.Backend]bool{}
	for i := 0; i < 9; i++ {
		seen[p.Pick(es, Key{}, 1)[0]] = true
	}
	if len(seen) != 3 {
		t.Fatalf("ties not rotated: %d", len(seen))
	}
}

func addrs(bs []*backend.Backend) []string {
	var o []string
	for _, b := range bs {
		o = append(o, b.Address())
	}
	return o
}

func TestSourceIPHashStable(t *testing.T) {
	snap := backend.NewSnapshot(pool(1, 1, 1, 1, 1))
	p := mustNew(t, "source_ip_hash", Options{})
	p.Rebuild(snap)
	for i := 0; i < 1000; i++ {
		a, b := p.Pick(snap, ipKey(i), 1)[0], p.Pick(snap, ipKey(i), 1)[0]
		if a != b {
			t.Fatalf("key %d unstable", i)
		}
	}
}

func TestSourceIPHashBoundedMovement(t *testing.T) {
	bs := pool(1, 1, 1, 1, 1, 1, 1, 1, 1, 1)
	full10 := backend.NewSnapshot(bs)
	less := backend.NewSnapshot(bs[:9])
	p := mustNew(t, "source_ip_hash", Options{})
	p.Rebuild(full10)
	const N = 20000
	before := make([]*backend.Backend, N)
	for i := range before {
		before[i] = p.Pick(full10, ipKey(i*7919), 1)[0]
	}
	p.Rebuild(less)
	moved := 0
	for i := range before {
		if p.Pick(less, ipKey(i*7919), 1)[0] != before[i] {
			moved++
		}
	}
	if f := float64(moved) / N; f > 0.15 {
		t.Fatalf("%.3f of keys moved", f)
	}
}

func TestSourceIPHashStableAcrossInstances(t *testing.T) {
	// distinct Backend objects, same endpoints
	s1 := backend.NewSnapshot(pool(1, 3, 20, 1))
	s2 := backend.NewSnapshot(pool(1, 3, 20, 1))
	p1, p2 := mustNew(t, "source_ip_hash", Options{}), mustNew(t, "source_ip_hash", Options{})
	p1.Rebuild(s1)
	p2.Rebuild(s2)
	for i := 0; i < 2000; i++ {
		a, b := p1.Pick(s1, ipKey(i), 2), p2.Pick(s2, ipKey(i), 2)
		if fmt.Sprint(addrs(a)) != fmt.Sprint(addrs(b)) {
			t.Fatalf("key %d: %v vs %v", i, addrs(a), addrs(b))
		}
	}
	v6 := Key{SourceIP: netip.MustParseAddr("2001:db8::1")}
	if p1.Pick(s1, v6, 1)[0].Address() != p2.Pick(s2, v6, 1)[0].Address() {
		t.Fatal("v6 disagrees")
	}
}

func TestRandomTwoChoices(t *testing.T) {
	bs := pool(1, 1, 1, 1)
	snap := backend.NewSnapshot(bs)
	for i := 0; i < 50; i++ {
		bs[0].TryAcquire()
	}
	p := mustNew(t, "random_two_choices", Options{})
	hits := map[*backend.Backend]int{}
	for i := 0; i < 4000; i++ {
		hits[p.Pick(snap, Key{}, 1)[0]]++
	}
	// the loaded backend wins only when both samples are it
	if hits[bs[0]] > 4000/16*2 {
		t.Fatalf("loaded backend picked %d times", hits[bs[0]])
	}
	for _, b := range bs[1:] {
		if hits[b] == 0 {
			t.Fatal("a backend was never picked")
		}
	}
	if got := p.Pick(snap, Key{}, 4); len(got) != 4 {
		t.Fatalf("want 4 distinct, got %d", len(got))
	}
}

func TestSlowStartRamp(t *testing.T) {
	b := mk(1, 100, 0)
	since := b.HealthySince()
	ss := 100 * time.Second
	for _, c := range []struct {
		el   time.Duration
		want int
	}{{0, 10}, {50 * time.Second, 55}, {100 * time.Second, 100}, {500 * time.Second, 100}, {-time.Second, 10}} {
		if got := EffectiveWeight(b, ss, since.Add(c.el)); got != c.want {
			t.Errorf("el=%v got %d want %d", c.el, got, c.want)
		}
	}
	if got := EffectiveWeight(mk(2, 1, 0), ss, since); got != 1 {
		t.Fatalf("floor: %d", got)
	}
	if got := EffectiveWeight(b, 0, since); got != 100 {
		t.Fatalf("disabled: %d", got)
	}
	// RR honours the ramp: fresh weight-10 backend vs established weight-10 one
	ss = 200 * time.Millisecond
	old := mk(3, 10, 0)
	time.Sleep(ss + 10*time.Millisecond) // old is past its ramp when fresh appears
	fresh := mk(4, 10, 0)
	now := fresh.HealthySince()
	clock := now
	p := mustNew(t, "round_robin", Options{SlowStart: ss, Now: func() time.Time { return clock }})
	snap := backend.NewSnapshot([]*backend.Backend{fresh, old})
	p.Rebuild(snap)
	// at t=0 fresh has weight 1 (10% of 10) vs 10 -> share 1/11
	n := 0
	for i := 0; i < 1100; i++ {
		if p.Pick(snap, Key{}, 1)[0] == fresh {
			n++
		}
	}
	if n < 80 || n > 120 {
		t.Fatalf("fresh share %d/1100 want ~100", n)
	}
	clock = now.Add(ss) // ramp done, lazy rebuild must pick it up
	n = 0
	for i := 0; i < 2000; i++ {
		if p.Pick(snap, Key{}, 1)[0] == fresh {
			n++
		}
	}
	if n < 900 || n > 1100 {
		t.Fatalf("post-ramp share %d/2000 want ~1000", n)
	}
}

func TestPickSkipsOverCap(t *testing.T) {
	for _, alg := range []string{"round_robin", "least_conn", "source_ip_hash", "random_two_choices"} {
		t.Run(alg, func(t *testing.T) {
			bs := []*backend.Backend{mk(1, 1, 1), mk(2, 1, 0), mk(3, 1, 1)}
			bs[0].TryAcquire()
			bs[2].TryAcquire()
			snap := backend.NewSnapshot(bs)
			p := mustNew(t, alg, Options{})
			p.Rebuild(snap)
			for i := 0; i < 200; i++ {
				got := p.Pick(snap, ipKey(i), 3)
				if len(got) != 1 || got[0] != bs[1] {
					t.Fatalf("got %v", addrs(got))
				}
			}
			bs[1] = nil
			all := backend.NewSnapshot([]*backend.Backend{bs[0], bs[2]})
			p.Rebuild(all) // the publisher rebuilds before the view is used
			if got := p.Pick(all, Key{}, 3); got != nil {
				t.Fatalf("all over cap should return nil, got %v", addrs(got))
			}
		})
	}
}

func TestPickDistinct(t *testing.T) {
	for _, alg := range []string{"round_robin", "least_conn", "source_ip_hash", "random_two_choices"} {
		t.Run(alg, func(t *testing.T) {
			snap := backend.NewSnapshot(pool(1, 5, 10, 2, 1))
			p := mustNew(t, alg, Options{})
			p.Rebuild(snap)
			for i := 0; i < 500; i++ {
				for _, n := range []int{1, 3, 5, 9} {
					got := p.Pick(snap, ipKey(i), n)
					if want := min(n, 5); len(got) != want {
						t.Fatalf("n=%d got %d", n, len(got))
					}
					seen := map[*backend.Backend]bool{}
					for _, b := range got {
						if seen[b] {
							t.Fatalf("duplicate in %v", addrs(got))
						}
						seen[b] = true
					}
				}
			}
			if p.Pick(&backend.Snapshot{}, Key{}, 3) != nil {
				t.Fatal("empty must be nil")
			}
		})
	}
}

func TestPickWithoutRebuildAndFailOpen(t *testing.T) {
	bs := pool(1, 1)
	bs[0].SetHealthy(false, backend.CauseProbe)
	bs[1].SetHealthy(false, backend.CauseProbe)
	snap := backend.NewSnapshot(bs)
	for _, alg := range []string{"round_robin", "source_ip_hash"} {
		p := mustNew(t, alg, Options{})
		p.Rebuild(snap)
		if p.Pick(snap, Key{}, 3) != nil {
			t.Fatal("no healthy: must return nil")
		}
		open := &backend.Snapshot{All: snap.All, Healthy: snap.All}
		p.Rebuild(open)
		if got := p.Pick(open, Key{}, 3); len(got) != 2 {
			t.Fatalf("%s fail-open got %d", alg, len(got))
		}
	}
}

func TestSourceIPHashRebuildNoopOnHealthChange(t *testing.T) {
	bs := pool(1, 2, 3, 4, 5)
	p := mustNew(t, "source_ip_hash", Options{}).(*sourceIPHash)
	p.Rebuild(backend.NewSnapshot(bs))
	st := p.state.Load()
	bs[2].SetHealthy(false, backend.CauseProbe)
	snap := backend.NewSnapshot(bs) // fresh All slice, same membership, one fewer healthy
	p.Rebuild(snap)
	if got := p.state.Load(); len(got.ring) != len(st.ring) || &got.ring[0] != &st.ring[0] {
		t.Fatal("ring was rebuilt on a health change")
	}
	if allocs := testing.AllocsPerRun(20, func() { p.Rebuild(snap) }); allocs != 0 {
		t.Fatalf("unchanged Rebuild allocates %v", allocs)
	}
	// a weight change is a membership change
	bs2 := pool(1, 2, 3, 4, 9)
	p.Rebuild(backend.NewSnapshot(bs2))
	if &p.state.Load().ring[0] == &st.ring[0] {
		t.Fatal("ring not rebuilt on weight change")
	}
}

func TestSourceIPHashSkipsUnhealthy(t *testing.T) {
	bs := pool(1, 1, 1, 1, 1)
	p := mustNew(t, "source_ip_hash", Options{})
	p.Rebuild(backend.NewSnapshot(bs))
	// reference: ring built from the 4 healthy backends alone
	ref := mustNew(t, "source_ip_hash", Options{})
	refSnap := backend.NewSnapshot(bs[1:])
	ref.Rebuild(refSnap)
	bs[0].SetHealthy(false, backend.CauseProbe)
	snap := backend.NewSnapshot(bs)
	for i := 0; i < 2000; i++ {
		got := p.Pick(snap, ipKey(i), 2)
		for _, b := range got {
			if b == bs[0] {
				t.Fatalf("key %d picked an unhealthy backend", i)
			}
		}
		if want := ref.Pick(refSnap, ipKey(i), 2); fmt.Sprint(addrs(got)) != fmt.Sprint(addrs(want)) {
			t.Fatalf("key %d: %v, healthy-only ring says %v", i, addrs(got), addrs(want))
		}
	}
}

func TestSourceIPHashFailOpen(t *testing.T) {
	bs := pool(1, 1, 1)
	for _, b := range bs {
		b.SetHealthy(false, backend.CauseProbe)
	}
	snap := backend.NewSnapshot(bs)
	p := mustNew(t, "source_ip_hash", Options{})
	open := &backend.Snapshot{All: snap.All, Healthy: snap.All}
	p.Rebuild(open)
	if p.Pick(snap, Key{}, 3) != nil {
		t.Fatal("no healthy and not fail-open: nil")
	}
	seen := map[*backend.Backend]bool{}
	for i := 0; i < 500; i++ {
		got := p.Pick(open, ipKey(i), 1)
		if len(got) != 1 {
			t.Fatal("fail-open must pick unhealthy backends")
		}
		seen[got[0]] = true
	}
	if len(seen) != 3 {
		t.Fatalf("fail-open reached %d backends", len(seen))
	}
}
