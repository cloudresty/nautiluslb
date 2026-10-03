//go:build integration

package integration

import (
	"testing"
)

func TestDiscoveryUpdatesBackends(t *testing.T) {
	addr := anyAddr
	a, b, c := newBackend(t, "A"), newBackend(t, "B"), newBackend(t, "C")
	e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone)), nil, tcpSvc(a, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 1)

	// serves reports the set of backends that answer n sequential connections.
	serves := func(n int) map[string]int {
		got := map[string]int{}
		for range n {
			cl, id := connectID(t, addr)
			_ = cl.Close()
			got[id]++
		}
		return got
	}

	if got := serves(3); got["A"] != 3 {
		t.Fatalf("single backend: %v", got)
	}

	// Add a Service: traffic reaches it.
	e.setService(tcpSvc(b, "web"))
	e.waitPool("web", "web", 2)
	if got := serves(10); got["A"] != 5 || got["B"] != 5 {
		t.Fatalf("after adding B: %v, want 5/5", got)
	}

	// Move Service A to backend C's port: traffic follows, A gets none.
	moved := tcpSvc(a, "web")
	moved.Spec.Ports[0].Port = int32(c.port)
	e.setService(moved)
	eventually(t, "traffic to follow the port change to C", func() bool {
		return serves(4)["C"] > 0
	})
	aBefore := a.accepted.Load()
	if got := serves(10); got["A"] != 0 || got["B"] != 5 || got["C"] != 5 {
		t.Fatalf("after moving A's Service to C's port: %v, want B:5 C:5", got)
	}
	if a.accepted.Load() != aBefore {
		t.Fatal("backend A is still being dialed after its address was removed")
	}

	// Remove B's Service: only C remains.
	e.deleteService("svc-B")
	e.waitPool("web", "web", 1)
	bBefore := b.accepted.Load()
	if got := serves(6); got["C"] != 6 {
		t.Fatalf("after removing B: %v, want all C", got)
	}
	if b.accepted.Load() != bBefore {
		t.Fatal("removed backend B is still being dialed")
	}

	// Delete the last Service: the pool is empty and connections are rejected
	// with no_backend rather than sent to a stale endpoint.
	e.deleteService("svc-A")
	e.waitPool("web", "web", 0)
	cBefore := c.accepted.Load()
	cl := dialLB(t, addr)
	if id, err := cl.id(); err == nil {
		t.Fatalf("connection reached backend %q although every Service was deleted", id)
	}
	e.waitMetric("rejected{no_backend}", 1, mp+"connections_rejected_total", "listener", "web", "reason", "no_backend")
	eventually(t, "no_backend access log record", func() bool {
		return e.log.count(func(r accesslogRecord) bool { return r.Listener == "web" && r.Result == "no_backend" }) >= 1
	})
	if c.accepted.Load() != cBefore {
		t.Fatal("a deleted backend was dialed from a stale pool")
	}

	// And it recovers when a Service comes back.
	e.setService(tcpSvc(a, "web"))
	e.waitPool("web", "web", 1)
	if got := serves(2); got["A"]+got["C"] != 2 {
		t.Fatalf("after re-adding a Service: %v", got)
	}
}

func TestPassiveEjectionAndRecovery(t *testing.T) {
	addr := anyAddr
	a, b, c := newBackend(t, "A"), newBackend(t, "B"), newBackend(t, "C")
	health := "    health: {type: tcp, interval: 100ms, timeout: 50ms, ejectionHold: 100ms, rise: 1, fall: 1}\n"
	e := startEnv(t, doc("2s", tcpConf("web", addr, health)), nil,
		tcpSvc(a, "web"), tcpSvc(b, "web"), tcpSvc(c, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 3)

	b.stop()
	// Right away, before any probe can have noticed: every connection is still
	// served, because a refused dial falls through to the next candidate.
	for i := range 9 {
		cl, id := connectID(t, addr)
		if id == "B" {
			t.Fatalf("connection %d served by the killed backend", i)
		}
		if !cl.echo("ok") {
			t.Fatalf("connection %d to %s did not echo", i, id)
		}
		_ = cl.Close()
	}
	e.waitPool("web", "web", 2)
	if n := e.metric(mp+"backend_health_transitions_total", "listener", "web", "to", "unhealthy"); n < 1 {
		t.Fatalf("health transitions to unhealthy = %v, want >= 1", n)
	}
	got := map[string]int{}
	for range 12 {
		cl, id := connectID(t, addr)
		_ = cl.Close()
		got[id]++
	}
	if got["B"] != 0 || got["A"] != 6 || got["C"] != 6 {
		t.Fatalf("with B ejected: %v, want A:6 C:6", got)
	}

	b.restart()
	e.waitPool("web", "web", 3)
	eventually(t, "recovered backend B to receive traffic again", func() bool {
		cl, id := connectID(t, addr)
		_ = cl.Close()
		return id == "B"
	})
}
