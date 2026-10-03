//go:build linux && 386

package pipe

import (
	"errors"
	"net"
)

// readTCPInfo is unsupported on linux/386, which has no direct
// getsockopt syscall (it goes through socketcall). Callers fall back to the
// generic path.
func readTCPInfo(*net.TCPConn) (sampler, error) { return nil, errors.ErrUnsupported }
