package sni

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type recConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *recConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf.Write(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

// hello returns the bytes a crypto/tls client writes first.
func hello(t testing.TB, serverName string) []byte {
	t.Helper()
	cli, srv := net.Pipe()
	defer func() { _ = srv.Close() }()
	rc := &recConn{Conn: cli}
	go func() {
		_ = tls.Client(rc, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
	}()
	// Read one record header then its body, so we stop exactly after the hello.
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		n, err := srv.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			t.Fatal(err)
		}
		if len(buf) >= 5 && len(buf) >= 5+int(binary.BigEndian.Uint16(buf[3:5])) {
			break
		}
	}
	_ = cli.Close()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]byte(nil), buf...)
}

func TestPeekRealClientHello(t *testing.T) {
	cli, srv := net.Pipe()
	defer func() { _ = srv.Close() }()
	rc := &recConn{Conn: cli}
	go func() {
		_ = tls.Client(rc, &tls.Config{ServerName: "Example.COM", InsecureSkipVerify: true}).Handshake()
	}()
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	name, consumed, err := Peek(srv, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if name != "example.com" {
		t.Fatalf("name %q", name)
	}
	rc.mu.Lock()
	written := append([]byte(nil), rc.buf.Bytes()...)
	rc.mu.Unlock()
	if !bytes.Equal(consumed, written) {
		t.Fatalf("consumed %d bytes, written %d", len(consumed), len(written))
	}
	_ = cli.Close()
}

func TestPeekBuffered(t *testing.T) {
	h := hello(t, "a.b.example.org.")
	extra := append(append([]byte(nil), h...), "APPDATA"...)
	r := bytes.NewReader(extra)
	name, consumed, err := Peek(r, 16384)
	if err != nil || name != "a.b.example.org" || !bytes.Equal(consumed, h) {
		t.Fatalf("%q %d %v", name, len(consumed), err)
	}
	if r.Len() != len("APPDATA") {
		t.Fatalf("over-read: %d left", r.Len())
	}
}

func splitRecords(h []byte, parts int) []byte {
	body := h[5:]
	size := (len(body) + parts - 1) / parts
	var out []byte
	for len(body) > 0 {
		n := min(size, len(body))
		out = append(out, h[0], h[1], h[2], byte(n>>8), byte(n))
		out = append(out, body[:n]...)
		body = body[n:]
	}
	return out
}

func TestPeekMultiRecord(t *testing.T) {
	h := hello(t, "multi.example.com")
	for _, parts := range []int{2, 3, 7} {
		in := splitRecords(h, parts)
		name, consumed, err := Peek(bytes.NewReader(in), 16384)
		if err != nil || name != "multi.example.com" || !bytes.Equal(consumed, in) {
			t.Fatalf("parts=%d: %q %v", parts, name, err)
		}
	}
	// hello header itself split across records (1 byte first)
	body := h[5:]
	in := []byte{h[0], h[1], h[2], 0, 1, body[0]}
	in = append(in, h[0], h[1], h[2], byte((len(body)-1)>>8), byte(len(body)-1))
	in = append(in, body[1:]...)
	if name, _, err := Peek(bytes.NewReader(in), 16384); err != nil || name != "multi.example.com" {
		t.Fatalf("%q %v", name, err)
	}
}

func TestPeekTooLarge(t *testing.T) {
	h := hello(t, "example.com")
	_, consumed, err := Peek(bytes.NewReader(h), len(h)-1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err %v", err)
	}
	if len(consumed) > len(h)-1 {
		t.Fatalf("consumed %d", len(consumed))
	}
	if _, _, err := Peek(bytes.NewReader(h), len(h)); err != nil {
		t.Fatal(err)
	}
}

func TestPeekNotTLS(t *testing.T) {
	_, consumed, err := Peek(strings.NewReader("GET / HTTP/1.1\r\n\r\n"), 4096)
	if !errors.Is(err, ErrNotTLS) {
		t.Fatalf("err %v", err)
	}
	if string(consumed) != "G" {
		t.Fatalf("consumed %q", consumed)
	}
	_, _, err = Peek(bytes.NewReader([]byte{0x16, 0x09, 0x09, 0, 5, 1, 2, 3, 4, 5}), 4096)
	if !errors.Is(err, ErrNotTLS) {
		t.Fatalf("bogus version: %v", err)
	}
}

func TestPeekNoSNI(t *testing.T) {
	h := hello(t, "")
	name, consumed, err := Peek(bytes.NewReader(h), 16384)
	if !errors.Is(err, ErrNoSNI) || name != "" {
		t.Fatalf("%q %v", name, err)
	}
	if !bytes.Equal(consumed, h) {
		t.Fatal("consumed must be full hello even without SNI")
	}
}

func TestPeekBadHostname(t *testing.T) {
	h := hello(t, "bad_name.example.com")
	if _, _, err := Peek(bytes.NewReader(h), 16384); err != nil {
		t.Fatalf("underscore tolerated: %v", err)
	}
	for _, in := range []string{"-a.com", "a..com", "a b.com", strings.Repeat("a", 64) + ".com"} {
		if _, err := normalize(in); !errors.Is(err, ErrNoSNI) {
			t.Fatalf("%q: %v", in, err)
		}
	}
}
