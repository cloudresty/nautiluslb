package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

type fakeReady struct {
	ok     bool
	reason string
}

func (f fakeReady) Ready() (bool, string) { return f.ok, f.reason }

func do(t *testing.T, s *Server, method, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code, rec.Body.String(), rec.Header()
}

func TestReadyz503WhenNotReady(t *testing.T) {
	s := New(Options{Readiness: fakeReady{false, "informers not synced"}})
	code, body, h := do(t, s, "GET", "/readyz")
	if code != 503 || !strings.Contains(body, "not ready") || !strings.Contains(body, "informers not synced") {
		t.Fatalf("got %d %q", code, body)
	}
	if h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", h)
	}
}

func TestReadyzOK(t *testing.T) {
	for _, r := range []Readiness{nil, fakeReady{true, ""}} {
		code, body, _ := do(t, New(Options{Readiness: r}), "GET", "/readyz")
		if code != 200 || !strings.Contains(body, `"ready"`) {
			t.Fatalf("got %d %q", code, body)
		}
	}
}

func TestHealthzAliases(t *testing.T) {
	s := New(Options{Readiness: fakeReady{true, ""}})
	for _, p := range []string{"/healthz", "/health/live"} {
		if code, body, _ := do(t, s, "GET", p); code != 200 || !strings.Contains(body, `"ok"`) {
			t.Fatalf("%s: %d %q", p, code, body)
		}
	}
	if code, _, _ := do(t, s, "GET", "/health/ready"); code != 200 {
		t.Fatalf("/health/ready: %d", code)
	}
}

func TestPprofOnlyWhenEnabled(t *testing.T) {
	if code, _, _ := do(t, New(Options{}), "GET", "/debug/pprof/"); code != 404 {
		t.Fatalf("disabled: %d", code)
	}
	if code, _, _ := do(t, New(Options{Pprof: true}), "GET", "/debug/pprof/"); code != 200 {
		t.Fatalf("opt: %d", code)
	}
	s := New(Options{Settings: config.AdminSettings{Pprof: true}})
	if code, _, _ := do(t, s, "GET", "/debug/pprof/"); code != 200 {
		t.Fatalf("settings: %d", code)
	}
}

func TestMetricsServesBuildInfo(t *testing.T) {
	reg := metrics.NewRegistry()
	metrics.RegisterBuildInfo(reg)
	code, body, _ := do(t, New(Options{Gatherer: reg}), "GET", "/metrics")
	if code != 200 || !strings.Contains(body, "nautiluslb_build_info") {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s := New(Options{})
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if code, _, _ := do(t, s, m, "/healthz"); code != 405 {
			t.Fatalf("%s: %d", m, code)
		}
	}
	if code, _, _ := do(t, s, "GET", "/nope"); code != 404 {
		t.Fatalf("404: %d", code)
	}
	if code, body, _ := do(t, s, "GET", "/"); code != 200 || !strings.Contains(body, "/metrics") {
		t.Fatalf("index: %d", code)
	}
}

func startServer(t *testing.T, addr string) *Server {
	t.Helper()
	s := New(Options{Settings: config.AdminSettings{Address: addr}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStartBindError(t *testing.T) {
	a := startServer(t, "127.0.0.1:0")
	defer func() { _ = a.Shutdown(context.Background()) }()
	b := New(Options{Settings: config.AdminSettings{Address: a.Addr()}})
	if err := b.Start(); err == nil {
		t.Fatal("expected bind error")
	}
}

func TestDisabledAddress(t *testing.T) {
	s := New(Options{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if s.Addr() != "" {
		t.Fatal("should not listen")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownStopsServing(t *testing.T) {
	s := startServer(t, "127.0.0.1:0")
	url := "http://" + s.Addr() + "/healthz"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	c := http.Client{Timeout: time.Second}
	if r, err := c.Get(url); err == nil {
		_ = r.Body.Close()
		t.Fatal("still serving")
	}
}
