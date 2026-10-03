//go:build linux && !386

package pipe

import (
	"errors"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// minTCPInfoLen is the TCP_INFO length that includes Notsent_bytes; kernels
// that report less (< 4.6) lack Bytes_acked/Bytes_received/Notsent_bytes.
var minTCPInfoLen = unsafe.Offsetof(unix.TCPInfo{}.Notsent_bytes) + 4

// readTCPInfo returns a sampler for conn, or errors.ErrUnsupported when the
// kernel does not expose the fields the watchdog needs.
func readTCPInfo(conn *net.TCPConn) (sampler, error) {

	rc, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}

	s := func() (sample, error) {
		var out sample
		var serr error
		cerr := rc.Control(func(fd uintptr) {
			var info unix.TCPInfo
			vallen := uint32(unix.SizeofTCPInfo)
			// Raw getsockopt instead of unix.GetsockoptTCPInfo: only the raw
			// call reports how many bytes the kernel filled (vallen), which
			// is how kernels older than 4.6 are detected. The buffer is the
			// full Go struct and vallen never exceeds its size.
			_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, unix.SOL_TCP, unix.TCP_INFO,
				uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&vallen)), 0) //nolint:gosec // G103: see above; bounded by SizeofTCPInfo
			if errno != 0 {
				serr = errno
				return
			}
			if uintptr(vallen) < minTCPInfoLen {
				serr = errors.ErrUnsupported
				return
			}
			out = sample{received: info.Bytes_received, acked: info.Bytes_acked, unacked: info.Unacked, notsent: info.Notsent_bytes}
		})
		if cerr != nil {
			return sample{}, cerr
		}
		return out, serr
	}

	// Probe once so an unsupported kernel selects the generic path.
	if _, err := s(); err != nil {
		return nil, err
	}
	return s, nil

}
