// Package pipe copies bytes between a client connection and an upstream
// connection, in both directions, until both have finished.
//
// Two copy paths share one skeleton (half-close forwarding, abort, panic
// containment):
//
//   - generic: io.CopyBuffer through an idle-aware reader and a stall-bounded
//     writer, bounds enforced in-band with socket deadlines (PR #8 semantics);
//   - splice (linux only): both connections are raw *net.TCPConn and a started
//     Watchdog is supplied, so Go hands the copy to splice(2) and the bounds are
//     enforced out-of-band by the Watchdog sampling TCP_INFO.
package pipe

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/emit"
)

const (
	// DefaultHalfCloseIdle bounds a connection after one side has finished
	// sending. Without it, a peer that never closes its half leaks the
	// connection and both goroutines for ever. It is an idle timeout, reset
	// by every byte, so a long response after a half-close still completes.
	DefaultHalfCloseIdle = 2 * time.Minute

	// DefaultWriteStall bounds one write to a peer that has stopped reading.
	// It applies only while there is data to deliver, so an idle connection
	// is never affected.
	DefaultWriteStall = 2 * time.Minute

	copyBufferSize = 32 * 1024

	ModeSplice  = "splice"
	ModeGeneric = "generic"

	EndedByClient  = "client"
	EndedByBackend = "backend"
	EndedByTimeout = "timeout"
	EndedByAbort   = "abort"
)

// Options configures Run.
type Options struct {
	// IdleTimeout aborts the pipe when no byte moves in either direction for
	// this long. 0 (the default) means an established connection never times
	// out.
	IdleTimeout time.Duration
	// PrefixToUpstream is written to upstream before any client byte is copied
	// (bytes already consumed from the client, e.g. a peeked ClientHello). It
	// is not counted in Result.BytesIn.
	PrefixToUpstream []byte
	// Watchdog enables the splice path (linux, both conns *net.TCPConn) and
	// supplies the half-close and write-stall bounds. Nil forces the generic
	// path with the default bounds.
	Watchdog *Watchdog
	// BufferPool supplies 32KB copy buffers for the generic path. Entries are
	// *[]byte; the pool's New func should return one. Nil allocates.
	BufferPool *sync.Pool

	// test hooks
	halfCloseIdle time.Duration
	writeStall    time.Duration
	forceGeneric  bool
}

// Result describes how a pipe ended.
type Result struct {
	// BytesIn is client to backend, BytesOut backend to client. On the splice
	// path a direction's count is only known when that direction finishes.
	BytesIn, BytesOut int64
	// Err is the first error that ended the pipe, nil on a clean finish of both
	// directions. Bound violations wrap os.ErrDeadlineExceeded; a recovered
	// panic is reported here.
	Err error
	// Mode is "splice" or "generic".
	Mode string
	// EndedBy: "client" or "backend" is the side that finished first (EOF) or
	// whose read/write failed on the generic path; "timeout" a bound fired;
	// "abort" a panic, a failed half-close, or (splice path) a copy error whose
	// side is not known.
	EndedBy string
}

var (
	errHalfCloseIdle = fmt.Errorf("pipe: half-closed connection idle too long: %w", os.ErrDeadlineExceeded)
	errWriteStall    = fmt.Errorf("pipe: peer stopped reading: %w", os.ErrDeadlineExceeded)
	errIdle          = fmt.Errorf("pipe: connection idle too long: %w", os.ErrDeadlineExceeded)
)

// sideErr tags a generic-path copy error with the side it came from.
type sideErr struct {
	side string
	err  error
}

func (e *sideErr) Error() string { return e.err.Error() }
func (e *sideErr) Unwrap() error { return e.err }

// session is the state shared by the two directions of one Run.
type session struct {
	client, upstream net.Conn
	halfCloseIdle    time.Duration
	writeStall       time.Duration
	idle             time.Duration
	generic          bool // deadline-based bounds (generic path)
	entry            *entry

	sealMu    sync.Mutex
	sealed    bool
	abortOnce sync.Once
	causeOnce sync.Once
	eofOnce   sync.Once
	endedBy   string // abort cause, valid when err != nil
	eofBy     string // side that finished first cleanly
	err       error

	halfClosed atomic.Bool
	lastActive atomic.Int64 // unix nanos, generic path IdleTimeout only
}

// Run copies client<->upstream until both directions have finished and
// returns. It blocks; it does not close either connection on a clean finish,
// the caller owns closing both.
//
// What Run closes: on any abort (a copy or half-close error, a recovered
// panic, a bound firing) it closes BOTH client and upstream, exactly as PR #8
// does, which unblocks the other direction at once. Closing again afterwards
// is harmless (net.ErrClosed). On a clean finish both directions have only
// been half-closed with CloseWrite (or Close when a conn has no CloseWrite).
//
// Run never sets a deadline on an established connection unless IdleTimeout
// is > 0, except the PR #8 bounds: the generic path sets a write deadline per
// write and, after a half-close, a read deadline; the splice path sets none.
func Run(client, upstream net.Conn, opts Options) Result {

	s := &session{client: client, upstream: upstream, idle: opts.IdleTimeout}

	s.halfCloseIdle, s.writeStall = DefaultHalfCloseIdle, DefaultWriteStall
	if w := opts.Watchdog; w != nil {
		s.halfCloseIdle, s.writeStall = w.halfCloseIdle, w.writeStall
	}
	if opts.halfCloseIdle > 0 {
		s.halfCloseIdle = opts.halfCloseIdle
	}
	if opts.writeStall > 0 {
		s.writeStall = opts.writeStall
	}

	mode := ModeGeneric
	var copyFn func(dst, src net.Conn, in bool) (int64, error)
	if !opts.forceGeneric {
		if e, ok := newEntry(client, upstream, opts.Watchdog); ok {
			s.entry = e
			e.idle = opts.IdleTimeout
			e.abort = s.abortTimeout
			mode = ModeSplice
			copyFn = spliceCopy
		}
	}
	if s.entry == nil {
		s.generic = true
		s.lastActive.Store(time.Now().UnixNano())
		copyFn = func(dst, src net.Conn, in bool) (int64, error) {
			return s.genericCopy(dst, src, in, opts.BufferPool)
		}
	} else {
		opts.Watchdog.register(s.entry)
	}

	var bytesIn, bytesOut atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.direction(upstream, client, true, copyFn, &bytesIn, opts.PrefixToUpstream)
	}()
	s.direction(client, upstream, false, copyFn, &bytesOut, nil)
	<-done

	// Both directions are done. Seal the cause so a late watchdog abort cannot
	// write err/endedBy or close the conns, then drop the watchdog entry.
	s.seal()
	if s.entry != nil {
		s.entry.closed.Store(true)
		opts.Watchdog.unregister(s.entry)
	}

	res := Result{BytesIn: bytesIn.Load(), BytesOut: bytesOut.Load(), Err: s.err, Mode: mode, EndedBy: s.endedBy}
	if s.err == nil {
		res.EndedBy = s.eofBy
	}
	return res
}

// setCause records the first abort cause only.
func (s *session) setCause(by string, err error) {
	s.causeOnce.Do(func() {
		s.endedBy, s.err = by, err
	})
}

// abort records the cause and closes both connections.
func (s *session) abort(by string, err error) {
	s.setCause(by, err)
	s.abortOnce.Do(func() {
		_ = s.client.Close()
		_ = s.upstream.Close()
	})
}

// abortTimeout is the watchdog's abort; it is a no-op once the session is sealed.
func (s *session) abortTimeout(err error) {
	s.sealMu.Lock()
	defer s.sealMu.Unlock()
	if s.sealed {
		return
	}
	s.abort(EndedByTimeout, err)
}

// seal waits for an in-flight watchdog abort and makes later ones no-ops.
func (s *session) seal() {
	s.sealMu.Lock()
	s.sealed = true
	s.sealMu.Unlock()
	s.causeOnce.Do(func() {})
}

// direction copies src to dst once, then forwards the half-close.
func (s *session) direction(dst, src net.Conn, in bool, copyFn func(dst, src net.Conn, in bool) (int64, error), count *atomic.Int64, prefix []byte) {

	name := "backend to client"
	finishedBy := EndedByBackend
	dstSide := EndedByClient
	if in {
		name, finishedBy, dstSide = "client to backend", EndedByClient, EndedByBackend
	}

	defer func() {
		if r := recover(); r != nil {
			emit.Error.StructuredFields("Recovered panic while copying data",
				emit.ZString("direction", name),
				emit.ZString("panic", fmt.Sprint(r)),
				emit.ZString("stack", string(debug.Stack())))
			s.abort(EndedByAbort, fmt.Errorf("pipe: panic copying %s: %v", name, r))
		}
	}()

	if len(prefix) > 0 {
		if _, err := s.write(dst, prefix); err != nil {
			s.abort(classify(err, dstSide), err)
			return
		}
	}

	n, err := copyFn(dst, src, in)
	count.Store(n)
	if err != nil {
		s.abort(classify(err, EndedByAbort), err)
		return
	}

	// Clean EOF: pass the half-close on, then bound the other direction.
	s.eofOnce.Do(func() { s.eofBy = finishedBy })
	s.halfClosed.Store(true)
	if s.entry != nil {
		s.entry.finished(in)
	}
	if err := closeWrite(dst); err != nil {
		s.abort(EndedByAbort, err)
		return
	}
	if s.generic {
		_ = dst.SetReadDeadline(time.Now().Add(s.halfCloseIdle))
	}

}

// write writes the prefix under the same stall bound as the generic path.
func (s *session) write(dst net.Conn, b []byte) (int, error) {
	if s.generic {
		w := &stallWriter{conn: dst, stall: s.writeStall, s: s}
		return w.Write(b)
	}
	return dst.Write(b)
}

// classify maps a copy error to Result.EndedBy.
func classify(err error, fallback string) string {
	if errors.Is(err, errHalfCloseIdle) || errors.Is(err, errWriteStall) || errors.Is(err, errIdle) || errors.Is(err, os.ErrDeadlineExceeded) {
		return EndedByTimeout
	}
	var se *sideErr
	if errors.As(err, &se) {
		return se.side
	}
	return fallback
}

func closeWrite(conn net.Conn) error {

	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}

	// No half-close available: closing is the only way to signal EOF.
	return conn.Close()

}

// genericCopy is PR #8's copy: io.CopyBuffer through idleReader/stallWriter.
func (s *session) genericCopy(dst, src net.Conn, in bool, pool *sync.Pool) (int64, error) {

	srcSide, dstSide := EndedByBackend, EndedByClient
	if in {
		srcSide, dstSide = EndedByClient, EndedByBackend
	}

	reader := &idleReader{conn: src, s: s, side: srcSide}
	writer := &stallWriter{conn: dst, stall: s.writeStall, s: s, side: dstSide}

	var buf []byte
	if pool != nil {
		if bp, ok := pool.Get().(*[]byte); ok && bp != nil && len(*bp) > 0 {
			buf = *bp
			defer pool.Put(bp)
		}
	}
	if buf == nil {
		buf = make([]byte, copyBufferSize)
	}

	_, err := io.CopyBuffer(writer, reader, buf)
	return writer.n, err

}

// idleReader applies the half-close bound to every read once either side has
// half-closed. Before that, reads have no deadline at all, unless the
// optional IdleTimeout is set.
type idleReader struct {
	conn net.Conn
	s    *session
	side string
}

func (r *idleReader) Read(b []byte) (int, error) {

	for {
		var limit time.Duration
		if r.s.halfClosed.Load() {
			limit = r.s.halfCloseIdle
		}
		if r.s.idle > 0 && (limit == 0 || r.s.idle < limit) {
			limit = r.s.idle
		}
		if limit > 0 {
			_ = r.conn.SetReadDeadline(time.Now().Add(limit))
		}

		n, err := r.conn.Read(b)
		if n > 0 {
			r.s.lastActive.Store(time.Now().UnixNano())
		}
		if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
			return n, wrapSide(err, r.side)
		}

		// A read deadline fired. Bytes moving in the other direction count
		// as activity for IdleTimeout, so only give up when truly idle.
		last := time.Unix(0, r.s.lastActive.Load())
		switch {
		case r.s.halfClosed.Load():
			return n, wrapTimeout(err, errHalfCloseIdle)
		case r.s.idle > 0 && time.Since(last) >= r.s.idle:
			return n, wrapTimeout(err, errIdle)
		case r.s.idle > 0 && n == 0:
			continue
		}
		return n, wrapSide(err, r.side)
	}

}

// stallWriter bounds each write by the write-stall timeout.
type stallWriter struct {
	conn  net.Conn
	stall time.Duration
	s     *session
	side  string
	n     int64
}

func (w *stallWriter) Write(b []byte) (int, error) {

	_ = w.conn.SetWriteDeadline(time.Now().Add(w.stall))
	n, err := w.conn.Write(b)
	w.n += int64(n)
	if n > 0 {
		w.s.lastActive.Store(time.Now().UnixNano())
	}
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		return n, wrapTimeout(err, errWriteStall)
	}
	return n, wrapSide(err, w.side)

}

func wrapSide(err error, side string) error {
	if err == nil || err == io.EOF {
		return err
	}
	return &sideErr{side: side, err: err}
}

func wrapTimeout(_ error, to error) error { return &sideErr{side: EndedByTimeout, err: to} }
