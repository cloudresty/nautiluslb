//go:build linux

package pipe

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkRunSplice(b *testing.B) { benchRun(b, ModeSplice) }

func TestModeSpliceOnLinux(t *testing.T) {
	var calls atomic.Int32
	var raw atomic.Bool
	spliceTestHook = func(dst *net.TCPConn, src io.Reader) {
		calls.Add(1)
		raw.Store(dst != nil) // dst is statically *net.TCPConn
		if _, ok := src.(*net.TCPConn); !ok {
			raw.Store(false)
		}
	}
	defer func() { spliceTestHook = nil }()

	r := newRig(t)
	opts := modeOpts(t, ModeSplice, 5*time.Second, 5*time.Second)
	res := runAsync(r.client, r.upstream, opts)
	go echoBackend(r.backendPeer)
	_, _ = r.clientPeer.Write([]byte("ping"))
	_ = r.clientPeer.CloseWrite()
	if got, _ := io.ReadAll(r.clientPeer); string(got) != "ping" {
		t.Fatalf("echo %q", got)
	}
	out := waitResult(t, res, 5*time.Second)
	if out.Mode != ModeSplice || out.Err != nil {
		t.Fatalf("result %+v", out)
	}
	if calls.Load() != 2 || !raw.Load() {
		t.Fatalf("hook calls=%d raw=%v", calls.Load(), raw.Load())
	}
}

// Non-TCPConn endpoints and a nil watchdog use the generic path on linux.
func TestFallbackToGenericOnLinux(t *testing.T) {
	r := newRig(t)
	type wrapped struct{ net.Conn }
	res := runAsync(r.client, wrapped{r.upstream}, Options{Watchdog: func() *Watchdog {
		w := NewWatchdog(0, 0)
		w.Start(context.Background())
		t.Cleanup(w.Stop)
		return w
	}()})
	_ = r.clientPeer.Close()
	_ = r.backendPeer.Close()
	if out := waitResult(t, res, 5*time.Second); out.Mode != ModeGeneric {
		t.Fatalf("mode %s", out.Mode)
	}
}

// A panic on the splice path is contained and aborts both directions.
func TestPanicInSpliceIsContained(t *testing.T) {
	spliceTestHook = func(dst *net.TCPConn, _ io.Reader) {
		if dst.RemoteAddr() != nil {
			panic("boom in splice")
		}
	}
	defer func() { spliceTestHook = nil }()

	r := newRig(t)
	res := runAsync(r.client, r.upstream, modeOpts(t, ModeSplice, 5*time.Second, 5*time.Second))
	out := waitResult(t, res, 3*time.Second)
	if out.Err == nil || out.EndedBy != EndedByAbort || out.Mode != ModeSplice {
		t.Fatalf("result %+v", out)
	}
}

// The watchdog interrupts ReadFrom blocked in splice by closing both conns,
// with SyscallConn().Control running concurrently (race detector).
func TestWatchdogAbortInterruptsBlockedSplice(t *testing.T) {
	r := newRig(t)
	opts := modeOpts(t, ModeSplice, 100*time.Millisecond, 10*time.Second)
	res := runAsync(r.client, r.upstream, opts)
	_ = r.clientPeer.CloseWrite()
	out := waitResult(t, res, 5*time.Second)
	if out.EndedBy != EndedByTimeout || out.Mode != ModeSplice {
		t.Fatalf("result %+v", out)
	}
}
