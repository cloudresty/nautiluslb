// Package accesslog writes one JSON line per proxied connection through a
// bounded queue and a single writer goroutine. It never blocks the data path.
package accesslog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

// Result vocabulary (fixed).
const (
	ResultOK           = "ok"
	ResultClientReset  = "client_reset"
	ResultBackendReset = "backend_reset"
	ResultTimeout      = "timeout"
	ResultIdleTimeout  = "idle_timeout"
	ResultNoBackend    = "no_backend"
	ResultDialFailed   = "dial_failed"
	ResultACL          = "acl"
	ResultLimit        = "limit"
	ResultProxyHeader  = "proxy_header"
	ResultSNINoRoute   = "sni_no_route"
	ResultSNIError     = "sni_error"
	ResultDraining     = "draining"
	ResultPanic        = "panic"
)

const defaultBuffer = 1024

// Record describes one finished connection. Client, Local and ProxySrc are host:port.
type Record struct {
	Time                     time.Time
	Listener, Protocol, Pool string
	Backend                  string
	Client, Local            string
	ProxySrc                 string
	SNI                      string
	Duration                 time.Duration
	BytesIn, BytesOut        int64
	Result, Error            string
	Mode                     string
}

// Logger accepts records without blocking.
type Logger interface{ Log(Record) }

type nopLogger struct{}

func (nopLogger) Log(Record) {}

// Nop returns a Logger that discards records.
func Nop() Logger { return nopLogger{} }

type line struct {
	Time       time.Time `json:"time"`
	Listener   string    `json:"listener"`
	Protocol   string    `json:"protocol"`
	Pool       string    `json:"pool,omitempty"`
	Backend    string    `json:"backend,omitempty"`
	Client     string    `json:"client,omitempty"`
	Local      string    `json:"local,omitempty"`
	ProxySrc   string    `json:"proxySrc,omitempty"`
	SNI        string    `json:"sni,omitempty"`
	DurationMs float64   `json:"durationMs"`
	BytesIn    int64     `json:"bytesIn"`
	BytesOut   int64     `json:"bytesOut"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
	Mode       string    `json:"mode,omitempty"`
}

type logger struct {
	ch  chan Record
	rec metrics.Recorder

	mu     sync.RWMutex // guards closed vs. sends on ch
	closed bool

	abort     chan struct{}
	abortOnce sync.Once
	done      chan struct{}
}

// New builds a Logger from settings. The returned close function stops
// accepting records and flushes what is queued until ctx is done; it is idempotent.
func New(s config.AccessLogSettings, rec metrics.Recorder) (Logger, func(ctx context.Context) error, error) {
	if !s.Enabled {
		return Nop(), func(context.Context) error { return nil }, nil
	}
	w, closer, err := open(s.Output)
	if err != nil {
		return nil, nil, err
	}
	l, closeFn := start(w, closer, s.BufferSize, rec)
	return l, closeFn, nil
}

func open(output string) (io.Writer, io.Closer, error) {
	switch output {
	case "", "stdout":
		return os.Stdout, nil, nil
	case "stderr":
		return os.Stderr, nil, nil
	}
	if !filepath.IsAbs(output) {
		return nil, nil, fmt.Errorf("accessLog output %q: must be stdout, stderr or an absolute path", output)
	}
	f, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("opening access log %q: %w", output, err)
	}
	return f, f, nil
}

func start(w io.Writer, closer io.Closer, size int, rec metrics.Recorder) (Logger, func(context.Context) error) {
	if size <= 0 {
		size = defaultBuffer
	}
	if rec == nil {
		rec = metrics.NewNop()
	}
	l := &logger{
		ch:    make(chan Record, size),
		rec:   rec,
		abort: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go l.run(w, closer)
	return l, l.close
}

// Log enqueues r; on a full queue (or after close) the record is dropped and counted.
func (l *logger) Log(r Record) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		l.rec.AccessLogDropped()
		return
	}
	select {
	case l.ch <- r:
	default:
		l.rec.AccessLogDropped()
	}
}

func (l *logger) close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.ch)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		l.abortOnce.Do(func() { close(l.abort) })
		return fmt.Errorf("flushing access log: %w", ctx.Err())
	}
}

func (l *logger) run(w io.Writer, closer io.Closer) {
	defer close(l.done)
	bw := bufio.NewWriter(w)
	defer func() {
		_ = bw.Flush()
		if closer != nil {
			_ = closer.Close()
		}
	}()
	enc := json.NewEncoder(bw) // appends the newline
	for r := range l.ch {
		select {
		case <-l.abort:
			continue // deadline passed: drain without writing so the goroutine exits
		default:
		}
		t := r.Time
		if t.IsZero() {
			t = time.Now()
		}
		_ = enc.Encode(line{
			Time: t.UTC(), Listener: r.Listener, Protocol: r.Protocol, Pool: r.Pool, Backend: r.Backend,
			Client: r.Client, Local: r.Local, ProxySrc: r.ProxySrc, SNI: r.SNI,
			DurationMs: float64(r.Duration) / float64(time.Millisecond),
			BytesIn:    r.BytesIn, BytesOut: r.BytesOut, Result: r.Result, Error: r.Error, Mode: r.Mode,
		})
		if len(l.ch) == 0 {
			_ = bw.Flush()
		}
	}
}
