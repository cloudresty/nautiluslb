package accesslog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/metrics"
)

type countRec struct {
	metrics.Recorder
	dropped atomic.Int64
}

func (c *countRec) AccessLogDropped() { c.dropped.Add(1) }

type slowWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	release chan struct{}
}

func (w *slowWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *slowWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func sample() Record {
	return Record{
		Time: time.Date(2026, 10, 2, 1, 2, 3, 4, time.UTC), Listener: "web", Protocol: "tcp",
		Pool: "web", Backend: "10.0.0.1:80", Client: "1.2.3.4:5555", Local: "10.0.0.9:443",
		Duration: 1500 * time.Microsecond, BytesIn: 7, BytesOut: 9, Result: ResultOK, Mode: "splice",
	}
}

func TestAccessLogDropsNotBlocks(t *testing.T) {
	rec := &countRec{Recorder: metrics.NewNop()}
	w := &slowWriter{release: make(chan struct{})}
	l, closeFn := start(w, nil, 1, rec)
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		l.Log(sample())
		if d := time.Since(t0); d > time.Millisecond {
			t.Fatalf("Log blocked %v", d)
		}
	}
	if rec.dropped.Load() == 0 {
		t.Fatal("expected drops")
	}
	close(w.release)
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAccessLogJSONFields(t *testing.T) {
	w := &slowWriter{release: make(chan struct{})}
	close(w.release)
	l, closeFn := start(w, nil, 8, nil)
	l.Log(sample())
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := w.String()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("want one line, got %q", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["time"] != "2026-10-02T01:02:03.000000004Z" || m["durationMs"] != 1.5 ||
		m["bytesIn"] != 7.0 || m["bytesOut"] != 9.0 || m["result"] != "ok" || m["client"] != "1.2.3.4:5555" {
		t.Errorf("unexpected fields: %v", m)
	}
	for _, k := range []string{"error", "sni", "proxySrc"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be omitted when empty", k)
		}
	}
}

func TestAccessLogCloseFlushes(t *testing.T) {
	w := &slowWriter{release: make(chan struct{})}
	close(w.release)
	l, closeFn := start(w, nil, 100, nil)
	for i := 0; i < 50; i++ {
		l.Log(sample())
	}
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closeFn(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if n := strings.Count(w.String(), "\n"); n != 50 {
		t.Errorf("flushed %d lines; want 50", n)
	}
	l.Log(sample()) // after close: must not panic
}

func TestAccessLogCloseDeadline(t *testing.T) {
	w := &slowWriter{release: make(chan struct{})}
	l, closeFn := start(w, nil, 4, nil)
	l.Log(sample())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := closeFn(ctx); err == nil {
		t.Fatal("expected deadline error")
	}
	close(w.release)
}

func TestAccessLogFileOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	l, closeFn, err := New(config.AccessLogSettings{Enabled: true, Output: path, BufferSize: 4}, metrics.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	l.Log(sample())
	if err := closeFn(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("file = %q, err %v", b, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640&^umask() {
		t.Logf("mode %v (umask-dependent)", st.Mode().Perm())
	}
	// appends across reopen
	l2, close2, _ := New(config.AccessLogSettings{Enabled: true, Output: path}, nil)
	l2.Log(sample())
	_ = close2(context.Background())
	b, _ = os.ReadFile(path)
	if strings.Count(string(b), "\n") != 2 {
		t.Errorf("not appended: %q", b)
	}
	if _, _, err := New(config.AccessLogSettings{Enabled: true, Output: "rel.log"}, nil); err == nil {
		t.Error("relative path must be rejected")
	}
}

func TestDisabledIsNop(t *testing.T) {
	l, closeFn, err := New(config.AccessLogSettings{}, nil)
	if err != nil || closeFn(context.Background()) != nil {
		t.Fatal(err)
	}
	l.Log(sample())
}

func umask() os.FileMode { return 0 }
