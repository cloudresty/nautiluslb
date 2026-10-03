//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/admin"
	"github.com/cloudresty/nautiluslb/internal/config"
)

func TestMetricsScrape(t *testing.T) {
	addr := anyAddr
	a, b := newBackend(t, "A"), newBackend(t, "B")
	e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone)), nil, tcpSvc(a, "web"), tcpSvc(b, "web"))
	addr = e.addr("web")
	e.waitPool("web", "web", 2)

	for range 6 {
		cl, _ := connectID(t, addr)
		if !cl.echo("hello") {
			t.Fatal("no echo")
		}
		_ = cl.Close()
	}
	e.waitMetric("closed connections with bytes", 1, mp+"connection_bytes_total", "listener", "web", "direction", "in")
	e.waitMetric("bytes out", 1, mp+"connection_bytes_total", "listener", "web", "direction", "out")

	checks := []struct {
		name string
		kv   []string
		min  float64
	}{
		{mp + "connections_accepted_total", []string{"listener", "web"}, 6},
		{mp + "connection_bytes_total", []string{"listener", "web", "direction", "in"}, 6 * 5},
		{mp + "backend_dial_total", []string{"listener", "web", "pool", "web", "result", "ok"}, 6},
		{mp + "pool_backends", []string{"listener", "web", "pool", "web", "state", "healthy"}, 2},
		{mp + "build_info", nil, 1},
		{mp + "ready", nil, 1},
	}
	for _, c := range checks {
		if got := e.metric(c.name, c.kv...); got < c.min {
			t.Errorf("%s%v = %v, want >= %v", c.name, c.kv, got, c.min)
		}
	}

	fams, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if !strings.HasPrefix(f.GetName(), mp) {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "client", "ip", "sni", "source", "src", "client_ip", "host":
					t.Errorf("%s carries the high-cardinality label %q", f.GetName(), l.GetName())
				}
				if strings.Contains(l.GetValue(), "example.test") {
					t.Errorf("%s label %s=%q carries an SNI name", f.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
}

func TestAdminEndpoints(t *testing.T) {
	addr := anyAddr
	be := newBackend(t, "A")
	g := newGatedSleep()
	e := startEnv(t, doc("2s", tcpConf("web", addr, healthNone)), g.opt, tcpSvc(be, "web"))
	t.Cleanup(g.open) // registered after startEnv, so it runs before the forced Shutdown
	addr = e.addr("web")
	e.waitPool("web", "web", 1)

	srv := admin.New(admin.Options{
		Settings:  config.AdminSettings{Address: "127.0.0.1:0"},
		Gatherer:  e.reg,
		Readiness: e.rt,
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	base := "http://" + srv.Addr()

	if code, body := httpGet(t, base+"/readyz"); code != 200 {
		t.Fatalf("/readyz = %d %s, want 200 when ready", code, body)
	}
	if code, _ := httpGet(t, base+"/healthz"); code != 200 {
		t.Fatalf("/healthz = %d, want 200", code)
	}
	cl, _ := connectID(t, addr)
	_ = cl.Close()
	e.waitMetric("accepted", 1, mp+"connections_accepted_total", "listener", "web")
	code, body := httpGet(t, base+"/metrics")
	if code != 200 || !strings.Contains(body, "nautiluslb_connections_accepted_total") || !strings.Contains(body, "nautiluslb_build_info") {
		t.Fatalf("/metrics = %d, missing nautiluslb families", code)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.rt.Shutdown(context.Background())
	}()
	g.waitStarted(t)
	// Inside the readiness delay: not ready, admin server still answering.
	code, body = httpGet(t, base+"/readyz")
	if code != 503 || !strings.Contains(body, "shutting down") {
		t.Fatalf("/readyz during the readiness delay = %d %s, want 503 shutting down", code, body)
	}
	if code, body := httpGet(t, base+"/metrics"); code != 200 || !strings.Contains(body, "nautiluslb_ready 0") {
		t.Fatalf("/metrics during shutdown = %d, want 200 with nautiluslb_ready 0", code)
	}
	g.open()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if code, _ := httpGet(t, base+"/readyz"); code != 503 {
		t.Fatalf("/readyz after shutdown = %d, want 503", code)
	}
}
