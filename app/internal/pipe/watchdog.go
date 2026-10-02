package pipe

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const defaultTick = 10 * time.Second

// sample is the part of TCP_INFO the watchdog needs for one socket.
type sample struct {
	received uint64 // Bytes_received: bytes the peer sent us
	acked    uint64 // Bytes_acked: bytes we sent that the peer acknowledged
	unacked  uint32 // segments in flight
	notsent  uint32 // bytes queued, not yet sent
}

func (s sample) pending() bool { return s.unacked > 0 || s.notsent > 0 }

// sampler reads one socket's sample. An error (typically a closed conn) makes
// the watchdog skip that pipe for the tick.
type sampler func() (sample, error)

// Direction masks for entry.done.
const (
	doneIn  = 1 // client to upstream finished
	doneOut = 2 // upstream to client finished
)

// entry is one splice-path pipe registered with the Watchdog. Fields below
// the "watchdog-owned" line are only touched by the watchdog goroutine.
type entry struct {
	sample [2]sampler // 0 client, 1 upstream
	idle   time.Duration
	abort  func(err error)
	done   atomic.Int32
	closed atomic.Bool // set by Run before unregister: check must not act any more

	// watchdog-owned
	init         bool
	prev         [2]sample
	lastActive   time.Time
	lastAck      [2]time.Time
	lastDirMoved [2]time.Time // [0] client to upstream, [1] upstream to client
	prevDone     int32
}

func (e *entry) finished(in bool) {
	if in {
		e.done.Or(doneIn)
	} else {
		e.done.Or(doneOut)
	}
}

// check enforces the three bounds for one tick at time now.
func (e *entry) check(now time.Time, halfCloseIdle, writeStall time.Duration) {

	if e.closed.Load() {
		return
	}

	var cur [2]sample
	for i := range cur {
		s, err := e.sample[i]()
		if err != nil {
			return
		}
		cur[i] = s
	}

	if !e.init {
		e.init = true
		e.prev = cur
		e.lastActive = now
		e.lastAck = [2]time.Time{now, now}
		e.lastDirMoved = [2]time.Time{now, now}
	}

	// A direction moved when its source received bytes or its destination
	// acknowledged bytes (a slow reader draining a buffer is progress).
	moved := [2]bool{
		cur[0].received != e.prev[0].received || cur[1].acked != e.prev[1].acked,
		cur[1].received != e.prev[1].received || cur[0].acked != e.prev[0].acked,
	}
	for i := range moved {
		if moved[i] {
			e.lastDirMoved[i] = now
			e.lastActive = now
		}
	}

	// Write stall: data queued for a peer whose acked counter does not move.
	for i := range cur {
		if !cur[i].pending() || cur[i].acked != e.prev[i].acked {
			e.lastAck[i] = now
		} else if now.Sub(e.lastAck[i]) >= writeStall {
			e.abort(ErrWriteStall)
			return
		}
	}

	// Half-close idle: once one direction finished, the open one must keep
	// moving. Its clock starts when the half-close is first seen.
	done := e.done.Load()
	if done != e.prevDone {
		e.prevDone = done
		e.lastDirMoved = [2]time.Time{now, now}
	}
	switch done {
	case doneIn: // only upstream to client remains
		if now.Sub(e.lastDirMoved[1]) >= halfCloseIdle {
			e.abort(ErrHalfCloseIdle)
			return
		}
	case doneOut:
		if now.Sub(e.lastDirMoved[0]) >= halfCloseIdle {
			e.abort(ErrHalfCloseIdle)
			return
		}
	}

	if e.idle > 0 && now.Sub(e.lastActive) >= e.idle {
		e.abort(ErrIdleTimeout)
		return
	}

	e.prev = cur
}

// Watchdog enforces the PR #8 bounds out-of-band for splice-path pipes: one
// goroutine, one registry, sampling TCP_INFO every tick. A pipe's bounds are
// accurate to within one tick.
type Watchdog struct {
	halfCloseIdle time.Duration
	writeStall    time.Duration
	tick          time.Duration // test hook

	mu      sync.Mutex
	entries map[*entry]struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running atomic.Bool
}

// NewWatchdog returns a stopped Watchdog. Zero arguments take the 2 minute
// defaults.
func NewWatchdog(halfCloseIdle, writeStall time.Duration) *Watchdog {
	if halfCloseIdle <= 0 {
		halfCloseIdle = DefaultHalfCloseIdle
	}
	if writeStall <= 0 {
		writeStall = DefaultWriteStall
	}
	return &Watchdog{
		halfCloseIdle: halfCloseIdle,
		writeStall:    writeStall,
		tick:          defaultTick,
		entries:       make(map[*entry]struct{}),
	}
}

// Start launches the sampling goroutine; it exits on ctx cancel or Stop.
// Pipes only take the splice path while the Watchdog is running. A second
// Start while running is a no-op.
func (w *Watchdog) Start(ctx context.Context) {

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running.Load() {
		return
	}
	ctx, w.cancel = context.WithCancel(ctx)
	w.running.Store(true)
	w.wg.Add(1)
	go w.loop(ctx)

}

// Stop stops the goroutine and waits for it to exit. Pipes already running on
// the splice path stay unbounded afterwards, so stop it only at shutdown.
func (w *Watchdog) Stop() {

	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	w.wg.Wait()

}

func (w *Watchdog) loop(ctx context.Context) {

	defer w.wg.Done()
	defer w.running.Store(false)

	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			w.sweep(now)
		}
	}

}

func (w *Watchdog) sweep(now time.Time) {

	w.mu.Lock()
	list := make([]*entry, 0, len(w.entries))
	for e := range w.entries {
		list = append(list, e)
	}
	w.mu.Unlock()

	for _, e := range list {
		e.check(now, w.halfCloseIdle, w.writeStall)
	}

}

func (w *Watchdog) register(e *entry) {
	w.mu.Lock()
	w.entries[e] = struct{}{}
	w.mu.Unlock()
}

func (w *Watchdog) unregister(e *entry) {
	w.mu.Lock()
	delete(w.entries, e)
	w.mu.Unlock()
}

// active reports whether pipes may rely on this watchdog.
func (w *Watchdog) active() bool { return w != nil && w.running.Load() }
