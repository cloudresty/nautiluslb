package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/config"
	nlbruntime "github.com/cloudresty/nautiluslb/internal/runtime"
)

func noEnv(string) string { return "" }

func TestValidateFlagExample(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--validate", "--config", "../../config.example.yaml"}, noEnv, &out); code != 0 {
		t.Fatalf("exit %d, output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "is valid") {
		t.Fatalf("no summary: %s", out.String())
	}
}

func TestValidateFlagInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("configurations: [not: valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"--validate", "--config", p}, noEnv, &out); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if code := run([]string{"--validate", "--config", filepath.Join(t.TempDir(), "missing.yaml")}, noEnv, &out); code != 1 {
		t.Fatalf("missing file exit %d, want 1", code)
	}
}

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--version"}, noEnv, &out); code != 0 || !strings.HasPrefix(out.String(), "nautiluslb ") {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

func TestReloadCoalesces(t *testing.T) {
	sigs := make(chan os.Signal, 16)
	var reloads atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	reload := func() {
		reloads.Add(1)
		started <- struct{}{}
		<-release
	}
	done := make(chan struct{})
	go func() {
		supervise(sigs, nil, reload, func(context.Context) {}, time.Second)
		close(done)
	}()

	sigs <- syscall.SIGHUP
	<-started
	for range 5 {
		sigs <- syscall.SIGHUP
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	time.Sleep(100 * time.Millisecond)
	sigs <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return")
	}
	if n := reloads.Load(); n > 2 {
		t.Fatalf("%d reloads, want at most 2", n)
	}
}

func TestSecondSignalForces(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	entered := make(chan struct{})
	var cancelled atomic.Bool
	shutdown := func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		cancelled.Store(true)
	}
	done := make(chan struct{})
	go func() {
		supervise(sigs, nil, func() {}, shutdown, time.Hour)
		close(done)
	}()
	sigs <- syscall.SIGTERM
	<-entered
	select {
	case <-done:
		t.Fatal("returned before the second signal")
	case <-time.After(100 * time.Millisecond):
	}
	sigs <- syscall.SIGINT
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second signal did not force shutdown")
	}
	if !cancelled.Load() {
		t.Fatal("shutdown context was not cancelled")
	}
}

func TestWatchFileTriggersReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 1)
	go watchFile(ctx, p, 10*time.Millisecond, out)
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(p, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, later, later)
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no change event")
	}
}

// blockingRuntime's Start blocks until its context is cancelled, like a
// runtime waiting for discovery to sync.
type blockingRuntime struct {
	started  chan struct{}
	shutdown atomic.Int32
}

func (b *blockingRuntime) Start(ctx context.Context) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingRuntime) Reload(*config.Config) (nlbruntime.Summary, error) {
	return nlbruntime.Summary{}, nil
}

func (b *blockingRuntime) Shutdown(context.Context) nlbruntime.Summary {
	b.shutdown.Add(1)
	return nlbruntime.Summary{}
}

type nopRec struct{}

func (nopRec) ConfigReload(string) {}

func TestSigtermDuringStart(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			rt := &blockingRuntime{started: make(chan struct{})}
			sigs := make(chan os.Signal, 4)
			var closed atomic.Bool
			code := make(chan int, 1)
			go func() {
				code <- lifecycle(rt, nopRec{}, &config.Config{}, "unused.yaml", sigs, func() { closed.Store(true) })
			}()
			<-rt.started
			sigs <- syscall.SIGHUP // must not be lost or end Start
			sigs <- sig
			select {
			case c := <-code:
				if c != 0 {
					t.Fatalf("exit %d, want 0", c)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("signal during Start was not acted on")
			}
			if rt.shutdown.Load() != 1 || !closed.Load() {
				t.Fatalf("shutdown calls = %d, closed = %v", rt.shutdown.Load(), closed.Load())
			}
		})
	}
}

func TestSighupDuringStartIsRequeued(t *testing.T) {
	rt := &blockingRuntime{started: make(chan struct{})}
	sigs := make(chan os.Signal, 4)
	bg, stop := context.WithCancel(context.Background())
	defer stop()
	type result struct {
		err error
		sig os.Signal
	}
	res := make(chan result, 1)
	go func() {
		sig, err := startOrSignal(rt, sigs, bg, stop)
		res <- result{err, sig}
	}()
	<-rt.started
	sigs <- syscall.SIGHUP
	time.Sleep(50 * time.Millisecond)
	stop() // Start ends on its own (not via a terminating signal)
	r := <-res
	if r.sig != nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("got %v, %v", r.err, r.sig)
	}
	select {
	case s := <-sigs:
		if s != syscall.SIGHUP {
			t.Fatalf("requeued %v", s)
		}
	default:
		t.Fatal("SIGHUP during Start was dropped")
	}
}
