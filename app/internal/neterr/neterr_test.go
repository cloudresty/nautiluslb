package neterr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

func wrapped(e syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", e)}
}

func TestClassify(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, dialErr := net.Dial("tcp", addr)
	if dialErr == nil {
		t.Skip("closed port unexpectedly accepted")
	}

	tests := []struct {
		name  string
		err   error
		want  Cause
		blame bool
		label string
	}{
		{"real refused dial", dialErr, CauseRefused, true, "refused"},
		{"refused wrapped", wrapped(syscall.ECONNREFUSED), CauseRefused, true, "refused"},
		{"bare errno", syscall.ECONNREFUSED, CauseRefused, true, "refused"},
		{"fmt wrapped", fmt.Errorf("x: %w", wrapped(syscall.ECONNREFUSED)), CauseRefused, true, "refused"},
		{"ehostunreach", wrapped(syscall.EHOSTUNREACH), CauseUnreachable, true, "unreachable"},
		{"enetunreach", wrapped(syscall.ENETUNREACH), CauseUnreachable, true, "unreachable"},
		{"etimedout", wrapped(syscall.ETIMEDOUT), CauseTimeout, true, "timeout"},
		{"ctx deadline", context.DeadlineExceeded, CauseTimeout, true, "timeout"},
		{"os deadline", os.ErrDeadlineExceeded, CauseTimeout, true, "timeout"},
		{"net timeout", &net.DNSError{IsTimeout: true}, CauseTimeout, true, "timeout"},
		{"emfile", wrapped(syscall.EMFILE), CauseLocal, false, "local"},
		{"enfile", wrapped(syscall.ENFILE), CauseLocal, false, "local"},
		{"eaddrnotavail", wrapped(syscall.EADDRNOTAVAIL), CauseLocal, false, "local"},
		{"eagain", wrapped(syscall.EAGAIN), CauseLocal, false, "local"},
		{"enobufs", wrapped(syscall.ENOBUFS), CauseLocal, false, "local"},
		{"eacces", wrapped(syscall.EACCES), CauseLocal, false, "local"},
		{"unknown", errors.New("boom"), CauseUnknown, false, "unknown"},
		{"nil", nil, CauseUnknown, false, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err)
			if got != tt.want {
				t.Fatalf("Classify = %v; want %v", got, tt.want)
			}
			if BackendAttributable(got) != tt.blame {
				t.Errorf("BackendAttributable = %v; want %v", !tt.blame, tt.blame)
			}
			if got.String() != tt.label {
				t.Errorf("String = %q; want %q", got.String(), tt.label)
			}
		})
	}
}
