// Package admin serves the operational HTTP endpoints: metrics, health,
// readiness and (optionally) pprof.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/cloudresty/emit"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// Readiness reports whether the process can take traffic and, if not, why.
type Readiness interface {
	Ready() (bool, string)
}

// Options configures a Server.
type Options struct {
	Settings  config.AdminSettings
	Pprof     bool
	Gatherer  prometheus.Gatherer
	Readiness Readiness
}

// Server is the admin HTTP server.
type Server struct {
	opts Options
	srv  *http.Server
	mux  *http.ServeMux

	mu   sync.Mutex
	ln   net.Listener
	done chan struct{}
}

// New builds the server and its routes; it does not listen.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	pprofOn := opts.Pprof || opts.Settings.Pprof

	s.mux.HandleFunc("/{$}", s.index(pprofOn))
	if opts.Gatherer != nil {
		s.mux.Handle("/metrics", metrics.Handler(opts.Gatherer))
	}
	s.mux.HandleFunc("/healthz", s.live)
	s.mux.HandleFunc("/health/live", s.live)
	s.mux.HandleFunc("/readyz", s.ready)
	s.mux.HandleFunc("/health/ready", s.ready)
	if pprofOn {
		s.mux.HandleFunc("/debug/pprof/", longWrite(pprof.Index))
		s.mux.HandleFunc("/debug/pprof/cmdline", longWrite(pprof.Cmdline))
		s.mux.HandleFunc("/debug/pprof/profile", longWrite(pprof.Profile))
		s.mux.HandleFunc("/debug/pprof/symbol", longWrite(pprof.Symbol))
		s.mux.HandleFunc("/debug/pprof/trace", longWrite(pprof.Trace))
	}

	s.srv = &http.Server{
		Handler:           s.guard(s.mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return s
}

// Handler exposes the routed handler (for tests and embedding).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Addr returns the bound address, or "" when not listening.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Start binds synchronously and serves in a goroutine owned by the server.
// An empty address disables the server.
func (s *Server) Start() error {
	addr := s.opts.Settings.Address
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin listen on %q: %w", addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()

	bound := ln.Addr().String()
	emit.Info.StructuredFields("Admin server listening", emit.ZString("addr", bound))
	if !isLoopback(ln.Addr()) {
		emit.Warn.StructuredFields("Admin address is not loopback; metrics and pprof are exposed",
			emit.ZString("addr", bound))
	}

	go func() {
		defer close(done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			emit.Error.StructuredFields("Admin server stopped",
				emit.ZString("error", err.Error()))
		}
	}()
	return nil
}

// Shutdown stops accepting, drains, and waits for the serve goroutine.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	select {
	case <-done:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	return err
}

func isLoopback(a net.Addr) bool {
	t, ok := a.(*net.TCPAddr)
	return ok && t.IP.IsLoopback()
}

// guard rejects non-GET/HEAD methods before routing.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// longWrite extends the write deadline for slow pprof captures.
func longWrite(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Readiness != nil {
		if ok, reason := s.opts.Readiness.Ready(); !ok {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"status": "not ready", "reason": reason})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) index(pprofOn bool) http.HandlerFunc {
	body := "nautiluslb admin\n\n/metrics\n/healthz\n/health/live\n/readyz\n/health/ready\n"
	if pprofOn {
		body += "/debug/pprof/\n"
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}
}
