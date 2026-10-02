package backend

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ep(port uint16) Endpoint {
	return Endpoint{Address: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), port), Weight: 1}
}

func TestSnapshotImmutable(t *testing.T) {
	a, b, c := New(ep(1), 0), New(ep(2), 0), New(ep(3), 0)
	b.SetHealthy(false, CauseProbe)
	in := []*Backend{a, b, c}
	s := NewSnapshot(in)
	in[0] = nil // caller mutating its slice must not leak in
	if len(s.All) != 3 || s.All[0] != a {
		t.Fatalf("All changed: %v", s.All)
	}
	if len(s.Healthy) != 2 || s.Healthy[0] != a || s.Healthy[1] != c {
		t.Fatalf("Healthy wrong: %v", s.Healthy)
	}
	a.SetHealthy(false, CausePassive) // later flips do not rewrite the snapshot
	if len(s.Healthy) != 2 || s.Healthy[0] != a {
		t.Fatal("snapshot followed a later health change")
	}
}

func TestSetHealthy(t *testing.T) {
	b := New(ep(1), 0)
	if !b.IsHealthy() || b.State() != Healthy {
		t.Fatal("new backend must be healthy")
	}
	if b.SetHealthy(true, CauseProbe) {
		t.Fatal("no-op reported as change")
	}
	if !b.SetHealthy(false, CausePassive) || b.IsHealthy() || b.LastCause() != CausePassive {
		t.Fatal("down transition")
	}
	b.SetEjectionHold(time.Minute)
	if b.EjectedUntil().Before(time.Now().Add(50 * time.Second)) {
		t.Fatal("hold not set")
	}
	before := b.HealthySince()
	time.Sleep(2 * time.Millisecond)
	if !b.SetHealthy(true, CauseProbe) || !b.HealthySince().After(before) {
		t.Fatal("up transition must restart HealthySince")
	}
	if !b.EjectedUntil().IsZero() {
		t.Fatal("hold must clear on recovery")
	}
	if got := b.Address(); got != "10.0.0.1:1" {
		t.Fatal(got)
	}
}

func TestTryAcquireCap(t *testing.T) {
	b := New(ep(1), 3)
	for i := 0; i < 3; i++ {
		if !b.TryAcquire() {
			t.Fatalf("acquire %d refused", i)
		}
	}
	if b.TryAcquire() {
		t.Fatal("acquired past cap")
	}
	b.Release()
	if !b.TryAcquire() || b.ActiveConnections() != 3 {
		t.Fatal("release did not free a slot")
	}
	u := New(ep(2), 0)
	for i := 0; i < 1000; i++ {
		if !u.TryAcquire() {
			t.Fatal("unlimited refused")
		}
	}
}

func TestTryAcquireCapRace(t *testing.T) {
	const max = 50
	b := New(ep(1), max)
	var granted, over atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.TryAcquire() {
				granted.Add(1)
				if b.ActiveConnections() > max {
					over.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if granted.Load() != max || over.Load() != 0 || b.ActiveConnections() != max {
		t.Fatalf("granted=%d over=%d active=%d", granted.Load(), over.Load(), b.ActiveConnections())
	}
}
