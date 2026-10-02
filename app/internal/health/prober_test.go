package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func hostOf(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

func TestHTTPProberStatus(t *testing.T) {

	var gotHost atomic.Value
	var redirected atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/err", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/followed", http.StatusFound)
	})
	mux.HandleFunc("/followed", func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 32<<10)
		for range 256 { // 8MB, far over the cap
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := hostOf(srv)
	ctx := context.Background()

	if err := NewHTTPProber("/ok", "", nil).Probe(ctx, addr); err != nil {
		t.Fatalf("200 should be ok: %v", err)
	}
	if err := NewHTTPProber("/err", "", nil).Probe(ctx, addr); err == nil {
		t.Fatal("500 should fail")
	}
	if err := NewHTTPProber("/err", "", []int{200, 500}).Probe(ctx, addr); err != nil {
		t.Fatalf("500 in expect should be ok: %v", err)
	}
	if err := NewHTTPProber("/redir", "", nil).Probe(ctx, addr); err == nil {
		t.Fatal("302 not in expect should fail")
	}
	if redirected.Load() {
		t.Fatal("redirect was followed")
	}
	if err := NewHTTPProber("/redir", "", []int{302}).Probe(ctx, addr); err != nil {
		t.Fatalf("302 in expect should be ok: %v", err)
	}

	if err := NewHTTPProber("/ok", "app.example.com", nil).Probe(ctx, addr); err != nil {
		t.Fatal(err)
	}
	if h, _ := gotHost.Load().(string); h != "app.example.com" {
		t.Fatalf("Host = %q, want override", h)
	}

	start := time.Now()
	if err := NewHTTPProber("/big", "", nil).Probe(ctx, addr); err != nil {
		t.Fatalf("large body: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("large body not bounded")
	}

	if err := NewHTTPProber("/ok", "", nil).Probe(ctx, "127.0.0.1:1"); err == nil {
		t.Fatal("refused connection should fail")
	}

}

func TestTCPProber(t *testing.T) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()

	p := NewTCPProber()
	if err := p.Probe(context.Background(), addr); err != nil {
		t.Fatalf("open port: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener saw no connection")
	}

	_ = ln.Close()
	if err := p.Probe(context.Background(), addr); err == nil {
		t.Fatal("closed port should fail")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Probe(ctx, addr); err == nil {
		t.Fatal("cancelled ctx should fail")
	}

}
