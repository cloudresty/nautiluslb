//go:build linux

package pipe

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cloudresty/emit"
)

// Go's TCPConn.ReadFrom falls back to a userspace copy only when splice fails
// with EINVAL, or when the pipe cannot be created (pipe2 failure surfaces as
// EINVAL from getPipe). Any other splice errno (EPERM from a seccomp filter,
// ENOSYS, EACCES) is "handled" and fails the connection with no data
// forwarded, so the process probes splice(2) once and disables the splice path
// if it is unusable.
var (
	spliceProbe = probeSplice // replaced in tests
	spliceOnce  sync.Once
	spliceOK    atomic.Bool
)

// spliceUsable runs the probe once per process and reports its verdict.
func spliceUsable() bool {
	spliceOnce.Do(func() {
		if err := spliceProbe(); err != nil {
			emit.Warn.StructuredFields("splice(2) unavailable, using the generic copy path for all connections",
				emit.ZString("error", err.Error()),
				emit.ZString("hint", "a seccomp/SystemCallFilter policy may block splice or pipe2"))
			return
		}
		spliceOK.Store(true)
	})
	return spliceOK.Load()
}

// probeSplice moves one byte from a loopback socket into a pipe with
// splice(2). EINVAL is acceptable: Go falls back to a userspace copy itself.
func probeSplice() error {
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fds[0]); _ = syscall.Close(fds[1]) }()
	// Everything is loopback, but bound it anyway: the probe runs on the
	// first eligible connection and must never stall it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = l.(*net.TCPListener).SetDeadline(dl)
	}
	a, err := (&net.Dialer{}).DialContext(ctx, "tcp", l.Addr().String())
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	b, err := l.Accept()
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	if _, err = a.Write([]byte{0}); err != nil {
		return err
	}
	rc, err := b.(*net.TCPConn).SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Read(func(fd uintptr) bool {
		_, serr = syscall.Splice(int(fd), nil, fds[1], nil, 1, 0x2) // SPLICE_F_NONBLOCK
		return serr != syscall.EAGAIN
	}); err != nil {
		return err
	}
	if errors.Is(serr, syscall.EINVAL) {
		return nil
	}
	return serr
}

// spliceTestHook, when set, is told what the splice path hands to ReadFrom.
// It exists so tests can assert the raw *net.TCPConn reaches ReadFrom with no
// wrapper in between (a wrapper would silently disable splice(2)).
var spliceTestHook func(dst *net.TCPConn, src io.Reader)

// newEntry selects the splice path: both conns must be raw *net.TCPConn, the
// watchdog must be running and the kernel must expose TCP_INFO byte counters.
func newEntry(client, upstream net.Conn, w *Watchdog) (*entry, bool) {

	if !w.active() {
		return nil, false
	}
	if !spliceUsable() {
		return nil, false
	}
	c, ok := client.(*net.TCPConn)
	if !ok {
		return nil, false
	}
	u, ok := upstream.(*net.TCPConn)
	if !ok {
		return nil, false
	}
	cs, err := readTCPInfo(c)
	if err != nil {
		return nil, false
	}
	us, err := readTCPInfo(u)
	if err != nil {
		return nil, false
	}
	return &entry{sample: [2]sampler{cs, us}}, true

}

// spliceCopy copies with dst.ReadFrom(src) on the RAW *net.TCPConn values so
// the runtime uses splice(2); no deadline is set, the Watchdog bounds the pipe
// and interrupts a blocked ReadFrom by closing both conns.
func spliceCopy(dst, src net.Conn, _ bool) (int64, error) {

	d, s := dst.(*net.TCPConn), src.(*net.TCPConn)
	if h := spliceTestHook; h != nil {
		h(d, s)
	}
	return d.ReadFrom(s)

}
