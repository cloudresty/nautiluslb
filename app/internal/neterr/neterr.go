// Package neterr classifies network errors into causes that decide whether a
// backend is to blame (passive ejection) or the local host is exhausted.
package neterr

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
)

// Cause is the classified reason of a network error.
type Cause uint8

const (
	CauseUnknown Cause = iota
	CauseRefused
	CauseUnreachable
	CauseTimeout
	CauseLocal
)

// Classify maps err onto a Cause; it sees through *net.OpError and *os.SyscallError.
func Classify(err error) Cause {
	if err == nil {
		return CauseUnknown
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return CauseRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return CauseUnreachable
	case errors.Is(err, syscall.ETIMEDOUT),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, os.ErrDeadlineExceeded):
		return CauseTimeout
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.EADDRNOTAVAIL), errors.Is(err, syscall.EAGAIN),
		errors.Is(err, syscall.ENOBUFS), errors.Is(err, syscall.EACCES):
		return CauseLocal
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return CauseTimeout
	}
	return CauseUnknown
}

// BackendAttributable reports whether c is evidence against the backend.
func BackendAttributable(c Cause) bool {
	return c == CauseRefused || c == CauseUnreachable || c == CauseTimeout
}

// String is the metric label value.
func (c Cause) String() string {
	switch c {
	case CauseRefused:
		return "refused"
	case CauseUnreachable:
		return "unreachable"
	case CauseTimeout:
		return "timeout"
	case CauseLocal:
		return "local"
	}
	return "unknown"
}
