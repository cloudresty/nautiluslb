package pipe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// rig is a client peer and a backend peer joined by a pipe:
// clientPeer <-> client | upstream <-> backendPeer, all real TCP.
type rig struct {
	clientPeer, client, upstream, backendPeer *net.TCPConn
}

func tcpPair(t testing.TB) (a, b *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		ch <- c
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-ch
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

func newRig(t testing.TB) *rig {
	r := &rig{}
	r.clientPeer, r.client = tcpPair(t)
	r.upstream, r.backendPeer = tcpPair(t)
	return r
}

// modeOpts returns Options forcing the given path with small bounds.
func modeOpts(t testing.TB, mode string, halfClose, stall time.Duration) Options {
	t.Helper()
	if mode == ModeGeneric {
		return Options{halfCloseIdle: halfClose, writeStall: stall}
	}
	w := NewWatchdog(halfClose, stall)
	w.tick = 20 * time.Millisecond
	w.Start(context.Background())
	t.Cleanup(w.Stop)
	return Options{Watchdog: w}
}

func eachMode(t *testing.T, fn func(t *testing.T, mode string)) {
	modes := []string{ModeGeneric}
	if runtime.GOOS == "linux" {
		modes = append(modes, ModeSplice)
	}
	for _, m := range modes {
		t.Run(m, func(t *testing.T) { fn(t, m) })
	}
}

func runAsync(c, u net.Conn, o Options) <-chan Result {
	ch := make(chan Result, 1)
	go func() { ch <- Run(c, u, o) }()
	return ch
}

func waitResult(t testing.TB, ch <-chan Result, d time.Duration) Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("Run did not return within %v\n%s", d, buf[:n])
		return Result{}
	}
}

// echoBackend echoes everything, then half-closes when the peer does.
func echoBackend(c *net.TCPConn) {
	_, _ = io.Copy(c, c)
	_ = c.CloseWrite()
}

func TestHalfCloseIsForwardedAndBothSidesFinish(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		r := newRig(t)
		opts := modeOpts(t, mode, 5*time.Second, 5*time.Second)
		res := runAsync(r.client, r.upstream, opts)
		go echoBackend(r.backendPeer)

		payload := bytes.Repeat([]byte("x"), 1<<20)
		go func() {
			_, _ = r.clientPeer.Write(payload)
			_ = r.clientPeer.CloseWrite()
		}()
		got, err := io.ReadAll(r.clientPeer)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("echoed %d bytes err=%v, want %d", len(got), err, len(payload))
		}
		out := waitResult(t, res, 5*time.Second)
		if out.Err != nil || out.Mode != mode || out.EndedBy != EndedByClient {
			t.Fatalf("result %+v", out)
		}
		if out.BytesIn != int64(len(payload)) || out.BytesOut != int64(len(payload)) {
			t.Fatalf("counts in=%d out=%d", out.BytesIn, out.BytesOut)
		}
	})
}

func TestErrorAbortsBoth(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		r := newRig(t)
		res := runAsync(r.client, r.upstream, modeOpts(t, mode, 5*time.Second, 5*time.Second))

		_ = r.backendPeer.SetLinger(0)
		_ = r.backendPeer.Close() // RST

		out := waitResult(t, res, 5*time.Second)
		if out.Err == nil || out.EndedBy == EndedByTimeout {
			t.Fatalf("result %+v", out)
		}
		_ = r.clientPeer.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := r.clientPeer.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("client not closed: %v", err)
		}
	})
}

type panicReadConn struct{ net.Conn }

func (panicReadConn) Read([]byte) (int, error) { panic("boom on read") }

type panicWriteConn struct{ net.Conn }

func (panicWriteConn) Write([]byte) (int, error) { panic("boom on write") }

func TestPanicInCopyIsContained(t *testing.T) {
	wrappers := map[string]func(net.Conn) net.Conn{
		"upstream read":  func(c net.Conn) net.Conn { return panicReadConn{c} },
		"upstream write": func(c net.Conn) net.Conn { return panicWriteConn{c} },
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			go func() { _, _ = io.Copy(io.Discard, r.backendPeer) }()
			res := runAsync(r.client, wrap(r.upstream), Options{})
			go func() { _, _ = r.clientPeer.Write([]byte("payload")) }()
			out := waitResult(t, res, 3*time.Second)
			if out.Err == nil || out.EndedBy != EndedByAbort {
				t.Fatalf("result %+v", out)
			}
		})
	}
}

func TestNoDeadlineOnEstablishedConn(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		const halfClose = 150 * time.Millisecond
		r := newRig(t)
		res := runAsync(r.client, r.upstream, modeOpts(t, mode, halfClose, halfClose))
		go echoBackend(r.backendPeer)

		time.Sleep(3 * halfClose)
		select {
		case out := <-res:
			t.Fatalf("silent connection ended: %+v", out)
		default:
		}
		if _, err := r.clientPeer.Write([]byte("late")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		_ = r.clientPeer.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(r.clientPeer, buf); err != nil || string(buf) != "late" {
			t.Fatalf("echo %q err=%v", buf, err)
		}
		_ = r.clientPeer.Close()
		_ = r.backendPeer.Close()
		waitResult(t, res, 5*time.Second)
	})
}

func TestHalfCloseIdleBound(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		const halfClose = 200 * time.Millisecond
		r := newRig(t)
		start := time.Now()
		res := runAsync(r.client, r.upstream, modeOpts(t, mode, halfClose, 10*time.Second))

		_ = r.clientPeer.CloseWrite() // backend never answers nor closes
		out := waitResult(t, res, 5*time.Second)
		if el := time.Since(start); el < halfClose {
			t.Fatalf("ended after %v, before the bound", el)
		}
		if out.EndedBy != EndedByTimeout || !errors.Is(out.Err, os.ErrDeadlineExceeded) {
			t.Fatalf("result %+v", out)
		}
		// The half-close must have been forwarded before the bound fired.
		_ = r.backendPeer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := r.backendPeer.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("backend saw %v, want EOF", err)
		}
	})
}

func TestHalfCloseIdleResetByTraffic(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		const halfClose = 300 * time.Millisecond
		r := newRig(t)
		res := runAsync(r.client, r.upstream, modeOpts(t, mode, halfClose, 10*time.Second))
		_ = r.clientPeer.CloseWrite()
		go func() { _, _ = io.Copy(io.Discard, r.clientPeer) }()

		for range 12 { // 12*100ms > 3x the bound, but never idle for the bound
			if _, err := r.backendPeer.Write([]byte("tick")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		select {
		case out := <-res:
			t.Fatalf("active half-closed connection ended: %+v", out)
		default:
		}
		_ = r.backendPeer.Close()
		out := waitResult(t, res, 5*time.Second)
		if out.Err != nil && out.EndedBy == EndedByTimeout {
			t.Fatalf("result %+v", out)
		}
	})
}

func TestWriteStallBound(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		const stall = 300 * time.Millisecond
		r := newRig(t)
		for _, c := range []*net.TCPConn{r.upstream, r.backendPeer} {
			_ = c.SetReadBuffer(4096)
			_ = c.SetWriteBuffer(4096)
		}
		res := runAsync(r.client, r.upstream, modeOpts(t, mode, 10*time.Second, stall))

		// backendPeer never reads, so upstream writes stall.
		go func() {
			chunk := bytes.Repeat([]byte("z"), 64<<10)
			for range 4096 {
				if _, err := r.clientPeer.Write(chunk); err != nil {
					return
				}
			}
		}()
		out := waitResult(t, res, 30*time.Second)
		if out.EndedBy != EndedByTimeout || !errors.Is(out.Err, os.ErrDeadlineExceeded) {
			t.Fatalf("result %+v", out)
		}
	})
}

func TestIdleTimeoutOptional(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		t.Run("fires", func(t *testing.T) {
			r := newRig(t)
			opts := modeOpts(t, mode, 10*time.Second, 10*time.Second)
			opts.IdleTimeout = 200 * time.Millisecond
			res := runAsync(r.client, r.upstream, opts)
			out := waitResult(t, res, 5*time.Second)
			if out.EndedBy != EndedByTimeout || !errors.Is(out.Err, os.ErrDeadlineExceeded) {
				t.Fatalf("result %+v", out)
			}
		})
		t.Run("one-way traffic keeps it alive", func(t *testing.T) {
			r := newRig(t)
			opts := modeOpts(t, mode, 10*time.Second, 10*time.Second)
			opts.IdleTimeout = 300 * time.Millisecond
			res := runAsync(r.client, r.upstream, opts)
			go func() { _, _ = io.Copy(io.Discard, r.backendPeer) }()
			for range 12 {
				if _, err := r.clientPeer.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			select {
			case out := <-res:
				t.Fatalf("busy connection ended: %+v", out)
			default:
			}
			_ = r.clientPeer.Close()
			_ = r.backendPeer.Close()
			waitResult(t, res, 5*time.Second)
		})
	})
}

func TestPrefixToUpstream(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		r := newRig(t)
		opts := modeOpts(t, mode, 5*time.Second, 5*time.Second)
		opts.PrefixToUpstream = []byte("HELLO")
		res := runAsync(r.client, r.upstream, opts)
		go func() {
			_, _ = r.clientPeer.Write([]byte("world"))
			_ = r.clientPeer.CloseWrite()
		}()
		got, err := io.ReadAll(r.backendPeer)
		if err != nil || string(got) != "HELLOworld" {
			t.Fatalf("backend got %q err=%v", got, err)
		}
		_ = r.backendPeer.Close()
		out := waitResult(t, res, 5*time.Second)
		if out.BytesIn != 5 {
			t.Fatalf("BytesIn=%d, want 5 (prefix excluded)", out.BytesIn)
		}
	})
}

func TestByteCounts(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		r := newRig(t)
		res := runAsync(r.client, r.upstream, modeOpts(t, mode, 5*time.Second, 5*time.Second))
		in, out := bytes.Repeat([]byte("a"), 100_000), bytes.Repeat([]byte("b"), 50_000)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.clientPeer.Write(in)
			_ = r.clientPeer.CloseWrite()
			_, _ = io.Copy(io.Discard, r.clientPeer)
		}()
		go func() {
			defer wg.Done()
			_, _ = r.backendPeer.Write(out)
			_ = r.backendPeer.CloseWrite()
			_, _ = io.Copy(io.Discard, r.backendPeer)
		}()
		got := waitResult(t, res, 5*time.Second)
		wg.Wait()
		if got.Err != nil || got.BytesIn != int64(len(in)) || got.BytesOut != int64(len(out)) || got.Mode != mode {
			t.Fatalf("result %+v", got)
		}
	})
}

func TestWatchdogStopWaits(t *testing.T) {
	w := NewWatchdog(0, 0)
	if w.halfCloseIdle != DefaultHalfCloseIdle || w.writeStall != DefaultWriteStall || w.tick != defaultTick {
		t.Fatalf("defaults: %+v", w)
	}
	w.Stop() // never started: no-op

	before := runtime.NumGoroutine()
	w.Start(context.Background())
	w.Start(context.Background()) // idempotent
	if !w.active() {
		t.Fatal("not running after Start")
	}
	w.Stop()
	if w.active() {
		t.Fatal("still running after Stop returned")
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines %d > %d after Stop", n, before)
	}

	// Stop on a cancelled context also leaves nothing behind.
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()
	w.Stop()
	if w.active() {
		t.Fatal("running after cancel+Stop")
	}

	// A stopped watchdog never selects the splice path.
	r := newRig(t)
	res := runAsync(r.client, r.upstream, Options{Watchdog: w})
	_ = r.clientPeer.Close()
	_ = r.backendPeer.Close()
	if out := waitResult(t, res, 5*time.Second); out.Mode != ModeGeneric {
		t.Fatalf("mode %s with stopped watchdog", out.Mode)
	}
}

// fakeSock drives entry.check without a kernel.
type fakeSock struct{ s sample }

func (f *fakeSock) sampler() sampler { return func() (sample, error) { return f.s, nil } }

func newFakeEntry(c, u *fakeSock, idle time.Duration) (*entry, *error) {
	var aborted error
	return &entry{sample: [2]sampler{c.sampler(), u.sampler()}, idle: idle, abort: func(err error) { aborted = err }}, &aborted
}

func TestEntryCheckBounds(t *testing.T) {
	t0 := time.Unix(1000, 0)
	const bound = time.Minute

	t.Run("write stall", func(t *testing.T) {
		c, u := &fakeSock{}, &fakeSock{}
		e, ab := newFakeEntry(c, u, 0)
		e.check(t0, bound, bound)
		u.s = sample{acked: 10, notsent: 500}
		e.check(t0.Add(10*time.Second), bound, bound) // acked moved: clock resets
		e.check(t0.Add(40*time.Second), bound, bound)
		if *ab != nil {
			t.Fatalf("aborted early: %v", *ab)
		}
		e.check(t0.Add(71*time.Second), bound, bound)
		if !errors.Is(*ab, ErrWriteStall) {
			t.Fatalf("abort = %v", *ab)
		}
	})
	t.Run("pending drained resets", func(t *testing.T) {
		c, u := &fakeSock{}, &fakeSock{}
		e, ab := newFakeEntry(c, u, 0)
		e.check(t0, bound, bound)
		u.s = sample{acked: 1, notsent: 1}
		e.check(t0.Add(50*time.Second), bound, bound)
		u.s = sample{acked: 1}
		e.check(t0.Add(100*time.Second), bound, bound)
		u.s = sample{acked: 1, notsent: 1}
		e.check(t0.Add(140*time.Second), bound, bound)
		if *ab != nil {
			t.Fatalf("aborted: %v", *ab)
		}
	})
	t.Run("half close idle", func(t *testing.T) {
		c, u := &fakeSock{}, &fakeSock{}
		e, ab := newFakeEntry(c, u, 0)
		e.check(t0, bound, bound)
		e.finished(true) // client finished; upstream to client remains
		e.check(t0.Add(10*time.Second), bound, bound)
		u.s = sample{received: 5} // backend still sending
		e.check(t0.Add(60*time.Second), bound, bound)
		e.check(t0.Add(100*time.Second), bound, bound)
		if *ab != nil {
			t.Fatalf("aborted while moving: %v", *ab)
		}
		e.check(t0.Add(125*time.Second), bound, bound)
		if !errors.Is(*ab, ErrHalfCloseIdle) {
			t.Fatalf("abort = %v", *ab)
		}
	})
	t.Run("idle", func(t *testing.T) {
		c, u := &fakeSock{}, &fakeSock{}
		e, ab := newFakeEntry(c, u, 30*time.Second)
		e.check(t0, bound, bound)
		c.s.received = 1
		e.check(t0.Add(20*time.Second), bound, bound)
		e.check(t0.Add(40*time.Second), bound, bound)
		if *ab != nil {
			t.Fatalf("aborted early: %v", *ab)
		}
		e.check(t0.Add(51*time.Second), bound, bound)
		if !errors.Is(*ab, ErrIdleTimeout) {
			t.Fatalf("abort = %v", *ab)
		}
	})
	t.Run("no idle bound by default", func(t *testing.T) {
		c, u := &fakeSock{}, &fakeSock{}
		e, ab := newFakeEntry(c, u, 0)
		for i := range 100 {
			e.check(t0.Add(time.Duration(i)*time.Hour), bound, bound)
		}
		if *ab != nil {
			t.Fatalf("aborted: %v", *ab)
		}
	})
}

// TestRunCompletionVsAbortRace hammers Run completion against late aborts.
func TestRunCompletionVsAbortRace(t *testing.T) {

	t.Run("generic", func(t *testing.T) {
		for range 50 {
			c1, c2 := net.Pipe()
			u1, u2 := net.Pipe()
			go func() { _ = c2.Close() }()
			go func() { _ = u2.Close() }()
			res := Run(c1, u1, Options{forceGeneric: true, halfCloseIdle: time.Microsecond, writeStall: time.Microsecond})
			_ = res.Err
			_ = res.EndedBy
		}
	})

	t.Run("check", func(t *testing.T) {
		for range 50 {
			c, u := net.Pipe()
			s := &session{client: c, upstream: u}
			// Stalled sockets: every check wants to abort.
			stalled := func() (sample, error) { return sample{unacked: 1}, nil }
			e := &entry{sample: [2]sampler{stalled, stalled}, abort: s.abortTimeout}
			s.entry = e

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				now := time.Now()
				for i := range 200 {
					e.check(now.Add(time.Duration(i)*time.Second), time.Nanosecond, time.Nanosecond)
				}
			}()

			s.seal()
			e.closed.Store(true)
			_, _ = s.err, s.endedBy
			wg.Wait()
			_ = c.Close()
			_ = u.Close()
		}
	})

}

// A complete exchange that ends with the backend already gone (FIN, then RST
// for the client's last message) must stay a clean finish: CloseWrite on the
// RST'd upstream returns ENOTCONN, which is not an abort.
func TestCloseWriteAfterBackendRSTIsClean(t *testing.T) {
	eachMode(t, func(t *testing.T, mode string) {
		r := newRig(t)
		done := runAsync(r.client, r.upstream, modeOpts(t, mode, 5*time.Second, 5*time.Second))

		if _, err := r.clientPeer.Write([]byte("request")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		if n, err := io.ReadFull(r.backendPeer, buf[:7]); err != nil || n != 7 {
			t.Fatalf("backend read: %d, %v", n, err)
		}
		if _, err := r.backendPeer.Write([]byte("response")); err != nil {
			t.Fatal(err)
		}
		_ = r.backendPeer.Close() // FIN

		if n, err := io.ReadFull(r.clientPeer, buf[:8]); err != nil || n != 8 {
			t.Fatalf("client read: %d, %v", n, err)
		}
		if _, err := r.clientPeer.Read(buf); err != io.EOF {
			t.Fatalf("client read after response = %v, want EOF", err)
		}

		// The final message reaches a closed socket: the backend answers RST.
		if _, err := r.clientPeer.Write([]byte("bye")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		_ = r.clientPeer.CloseWrite()

		res := waitResult(t, done, 5*time.Second)
		if res.Err != nil || res.EndedBy == EndedByAbort {
			t.Fatalf("Err = %v, EndedBy = %q; want a clean finish", res.Err, res.EndedBy)
		}
		if res.BytesOut != 8 {
			t.Errorf("BytesOut = %d, want 8", res.BytesOut)
		}
	})
}
