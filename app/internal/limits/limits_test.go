package limits

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGateUnlimitedZero(t *testing.T) {
	g := NewGate(0)
	for i := 0; i < 1000; i++ {
		if !g.TryAcquire() {
			t.Fatal("unlimited gate refused")
		}
	}
	if g.Active() != 1000 {
		t.Fatalf("active=%d", g.Active())
	}
}

func TestGateCapacity(t *testing.T) {
	g := NewGate(2)
	for i := range 2 {
		if !g.TryAcquire() {
			t.Fatalf("acquire %d refused below capacity", i+1)
		}
	}
	if g.TryAcquire() {
		t.Fatal("capacity not enforced")
	}
	g.Release()
	if !g.TryAcquire() {
		t.Fatal("slot not reusable")
	}
	g.Release()
	g.Release()
	g.Release() // underflow clamps
	if g.Active() != 0 {
		t.Fatalf("active=%d", g.Active())
	}
}

func TestGateRace(t *testing.T) {
	const capN = 50
	g := NewGate(capN)
	var max atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if g.TryAcquire() {
					a := g.Active()
					for {
						m := max.Load()
						if a <= m || max.CompareAndSwap(m, a) {
							break
						}
					}
					g.Release()
				}
			}
		}()
	}
	wg.Wait()
	if max.Load() > capN {
		t.Fatalf("max active %d > %d", max.Load(), capN)
	}
	if g.Active() != 0 {
		t.Fatalf("active=%d", g.Active())
	}
}

func TestGateSetCapacityLower(t *testing.T) {
	g := NewGate(5)
	for i := 0; i < 5; i++ {
		g.TryAcquire()
	}
	g.SetCapacity(2)
	if g.TryAcquire() {
		t.Fatal("acquired above lowered capacity")
	}
	g.Release()
	g.Release()
	g.Release() // active 2 == capacity: still refused
	if g.TryAcquire() {
		t.Fatal("acquired at capacity")
	}
	g.Release() // active 1
	if !g.TryAcquire() || g.Active() != 2 {
		t.Fatalf("active=%d", g.Active())
	}
}

func TestPerSourceCapacity(t *testing.T) {
	p := NewPerSource(2)
	a, b := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")
	for i := range 2 {
		if !p.TryAcquire(a) {
			t.Fatalf("acquire %d refused below capacity", i+1)
		}
	}
	if p.TryAcquire(a) {
		t.Fatal("per-ip capacity not enforced")
	}
	if !p.TryAcquire(b) {
		t.Fatal("other ip blocked")
	}
}

func TestPerSourceCleanup(t *testing.T) {
	p := NewPerSource(3)
	var ips []netip.Addr
	for i := 0; i < 200; i++ {
		ip := netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i)})
		ips = append(ips, ip)
		p.TryAcquire(ip)
		p.TryAcquire(ip)
	}
	if p.len() != 200 {
		t.Fatalf("len=%d", p.len())
	}
	for _, ip := range ips {
		p.Release(ip)
		p.Release(ip)
		p.Release(ip) // extra no-op
	}
	if p.len() != 0 {
		t.Fatalf("len=%d after release", p.len())
	}
}

func TestPerSourceUnlimitedNoGrowth(t *testing.T) {
	p := NewPerSource(0)
	for i := 0; i < 1000; i++ {
		if !p.TryAcquire(netip.AddrFrom4([4]byte{1, 2, byte(i / 256), byte(i)})) {
			t.Fatal("refused")
		}
	}
	if p.len() != 0 {
		t.Fatalf("len=%d", p.len())
	}
}

func TestPerSourceIPv4Mapped(t *testing.T) {
	p := NewPerSource(1)
	v4 := netip.MustParseAddr("192.0.2.1")
	m := netip.MustParseAddr("::ffff:192.0.2.1")
	if !p.TryAcquire(v4) || p.TryAcquire(m) {
		t.Fatal("mapped address counted separately")
	}
	p.Release(m)
	if p.len() != 0 {
		t.Fatal("release via mapped form failed")
	}
}

func TestPerSourceRace(t *testing.T) {
	const capN = 3
	p := NewPerSource(capN)
	ip := netip.MustParseAddr("2001:db8::1")
	var cur, bad atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if p.TryAcquire(ip) {
					if cur.Add(1) > capN {
						bad.Add(1)
					}
					cur.Add(-1)
					p.Release(ip)
				}
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatal("capacity exceeded")
	}
	if p.len() != 0 {
		t.Fatalf("len=%d", p.len())
	}
}

func BenchmarkGate(b *testing.B) {
	g := NewGate(1 << 20)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if g.TryAcquire() {
				g.Release()
			}
		}
	})
}

func BenchmarkPerSource(b *testing.B) {
	p := NewPerSource(1 << 20)
	ip := netip.MustParseAddr("10.1.2.3")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if p.TryAcquire(ip) {
				p.Release(ip)
			}
		}
	})
}
