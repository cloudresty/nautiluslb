//go:build linux || darwin

package tcpproxy

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sockInt reads an integer socket option from a TCP connection.
func sockInt(t *testing.T, c net.Conn, level, opt int) int {
	t.Helper()
	sc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var gerr error
	if err := sc.Control(func(fd uintptr) { v, gerr = unix.GetsockoptInt(int(fd), level, opt) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatalf("getsockopt(%d,%d): %v", level, opt, gerr)
	}
	return v
}

func assertKeepAlive(t *testing.T, c net.Conn) {
	t.Helper()
	if sockInt(t, c, unix.SOL_SOCKET, unix.SO_KEEPALIVE) == 0 {
		t.Fatal("SO_KEEPALIVE not enabled")
	}
	if got := sockInt(t, c, unix.IPPROTO_TCP, keepIdleOpt); got != 30 {
		t.Errorf("keepalive idle = %d, want 30", got)
	}
	if got := sockInt(t, c, unix.IPPROTO_TCP, unix.TCP_KEEPINTVL); got != 10 {
		t.Errorf("keepalive interval = %d, want 10", got)
	}
	if got := sockInt(t, c, unix.IPPROTO_TCP, unix.TCP_KEEPCNT); got != 3 {
		t.Errorf("keepalive count = %d, want 3", got)
	}
}

func TestAcceptedConnHasKeepAlive(t *testing.T) {
	h := newTP(t, tpConfig(), nil, nil)
	if err := h.l.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.l.ln.Close() })
	client, err := net.Dial("tcp", h.l.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	accepted, err := h.l.ln.Accept() // production listener, no Serve
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	assertKeepAlive(t, accepted)
}

func TestDefaultDialerHasKeepAlive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := defaultDial()(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	assertKeepAlive(t, c)
}
