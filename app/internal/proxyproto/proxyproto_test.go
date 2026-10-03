package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func tcp(s string) net.Addr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }
func udp(s string) net.Addr { return net.UDPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		v        Version
		src, dst net.Addr
		want     string
	}{
		{"v1 tcp4", V1, tcp("192.0.2.1:1234"), tcp("198.51.100.2:443"), TCP4},
		{"v1 tcp6", V1, tcp("[2001:db8::1]:1234"), tcp("[2001:db8::2]:443"), TCP6},
		{"v1 mapped", V1, tcp("[::ffff:192.0.2.1]:1"), tcp("[::ffff:192.0.2.9]:2"), TCP4},
		{"v2 tcp4", V2, tcp("192.0.2.1:1234"), tcp("198.51.100.2:443"), TCP4},
		{"v2 tcp6", V2, tcp("[2001:db8::1]:65535"), tcp("[2001:db8::2]:0"), TCP6},
		{"v2 udp4", V2, udp("192.0.2.1:53"), udp("198.51.100.2:53"), UDP4},
		{"v2 udp6", V2, udp("[2001:db8::1]:53"), udp("[2001:db8::2]:53"), UDP6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteHeader(&buf, c.v, c.src, c.dst); err != nil {
				t.Fatal(err)
			}
			buf.WriteString("payload")
			br := bufio.NewReader(&buf)
			h, err := ReadHeader(br)
			if err != nil {
				t.Fatal(err)
			}
			_, sap, _ := endpoint(c.src)
			_, dap, _ := endpoint(c.dst)
			if h.Version != c.v || h.Transport != c.want || h.Src != netip.AddrPortFrom(sap.Addr().Unmap(), sap.Port()) ||
				h.Dst != netip.AddrPortFrom(dap.Addr().Unmap(), dap.Port()) || h.Local {
				t.Fatalf("got %+v", h)
			}
			rest, _ := io.ReadAll(br)
			if string(rest) != "payload" {
				t.Fatalf("rest %q", rest)
			}
		})
	}
}

func TestV1Format(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteHeader(&buf, V1, tcp("192.0.2.1:1234"), tcp("198.51.100.2:443"))
	if buf.String() != "PROXY TCP4 192.0.2.1 198.51.100.2 1234 443\r\n" {
		t.Fatalf("%q", buf.String())
	}
}

func TestNonIPUnknown(t *testing.T) {
	unix := &net.UnixAddr{Name: "/x", Net: "unix"}
	var b1, b2 bytes.Buffer
	_ = WriteHeader(&b1, V1, unix, unix)
	if b1.String() != "PROXY UNKNOWN\r\n" {
		t.Fatalf("%q", b1.String())
	}
	_ = WriteHeader(&b2, V2, unix, unix)
	h, err := ReadHeader(bufio.NewReader(&b2))
	if err != nil || h.Transport != UNKNOWN || h.Local {
		t.Fatalf("%+v %v", h, err)
	}
	h, err = ReadHeader(bufio.NewReader(&b1))
	if err != nil || h.Transport != UNKNOWN {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestReadHeaderNotProxy(t *testing.T) {
	in := "GET / HTTP/1.1\r\nHost: x\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(in))
	if _, err := ReadHeader(br); !errors.Is(err, ErrNotProxy) {
		t.Fatalf("err %v", err)
	}
	got, _ := io.ReadAll(br)
	if string(got) != in {
		t.Fatalf("consumed bytes: %q", got)
	}
}

func TestV1Malformed(t *testing.T) {
	long := "PROXY TCP4 1.1.1.1 2.2.2.2 1 2" + strings.Repeat(" ", 100) + "\r\n"
	cases := map[string]string{
		"no crlf":          "PROXY TCP4 1.1.1.1 2.2.2.2 1 2\n",
		"too long":         long,
		"truncated":        "PROXY TCP4 1.1.1.1",
		"bad proto":        "PROXY UDP4 1.1.1.1 2.2.2.2 1 2\r\n",
		"few fields":       "PROXY TCP4 1.1.1.1 2.2.2.2 1\r\n",
		"many fields":      "PROXY TCP4 1.1.1.1 2.2.2.2 1 2 3\r\n",
		"port overflow":    "PROXY TCP4 1.1.1.1 2.2.2.2 65536 2\r\n",
		"port plus":        "PROXY TCP4 1.1.1.1 2.2.2.2 +1 2\r\n",
		"port neg":         "PROXY TCP4 1.1.1.1 2.2.2.2 -1 2\r\n",
		"port leading 0":   "PROXY TCP4 1.1.1.1 2.2.2.2 01 2\r\n",
		"port empty":       "PROXY TCP4 1.1.1.1 2.2.2.2  2\r\n",
		"family mismatch":  "PROXY TCP4 ::1 ::2 1 2\r\n",
		"family mismatch6": "PROXY TCP6 1.1.1.1 2.2.2.2 1 2\r\n",
		"bad ip":           "PROXY TCP4 1.1.1 2.2.2.2 1 2\r\n",
		"double space":     "PROXY TCP4  1.1.1.1 2.2.2.2 1 2\r\n",
		"mapped in tcp4":   "PROXY TCP4 ::ffff:1.1.1.1 2.2.2.2 1 2\r\n",
		"bare":             "PROXY \r\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadHeader(bufio.NewReader(strings.NewReader(in))); !errors.Is(err, ErrMalformed) {
				t.Fatalf("err %v", err)
			}
		})
	}
	// Longest legal v1 header (107 bytes) is accepted.
	ok := "PROXY TCP6 ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff 65535 65535\r\n"
	if len(ok) != 104 {
		t.Logf("len %d", len(ok))
	}
	if _, err := ReadHeader(bufio.NewReader(strings.NewReader(ok))); err != nil {
		t.Fatal(err)
	}
}

func v2(cmd, fp byte, payload []byte) []byte {
	b := []byte(v2Sig)
	b = append(b, cmd, fp)
	b = binary.BigEndian.AppendUint16(b, uint16(len(payload)))
	return append(b, payload...)
}

func TestV2Truncated(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteHeader(&buf, V2, tcp("192.0.2.1:1"), tcp("192.0.2.2:2"))
	full := buf.Bytes()
	for n := 1; n < len(full); n++ {
		_, err := ReadHeader(bufio.NewReader(bytes.NewReader(full[:n])))
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("n=%d err %v", n, err)
		}
	}
}

func TestV2LengthCap(t *testing.T) {
	b := v2(0x21, 0x11, nil)
	binary.BigEndian.PutUint16(b[14:], 1025)
	b = append(b, make([]byte, 1025)...)
	if _, err := ReadHeader(bufio.NewReader(bytes.NewReader(b))); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err %v", err)
	}
	// exactly 1024 with TLVs is fine
	p := append([]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 1, 0, 2}, make([]byte, 1024-12)...)
	if _, err := ReadHeader(bufio.NewReader(bytes.NewReader(v2(0x21, 0x11, p)))); err != nil {
		t.Fatal(err)
	}
}

func TestV2Local(t *testing.T) {
	b := append(v2(0x20, 0x00, []byte{9, 9, 9}), "next"...)
	br := bufio.NewReader(bytes.NewReader(b))
	h, err := ReadHeader(br)
	if err != nil || !h.Local || h.Version != V2 {
		t.Fatalf("%+v %v", h, err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != "next" {
		t.Fatalf("%q", rest)
	}
}

func TestV2TLVsSkipped(t *testing.T) {
	p := []byte{10, 0, 0, 1, 10, 0, 0, 2, 0x30, 0x39, 0x01, 0xbb}
	p = append(p, 0x04, 0x00, 0x03, 'a', 'b', 'c') // TLV
	br := bufio.NewReader(bytes.NewReader(append(v2(0x21, 0x11, p), "x"...)))
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	if h.Src != netip.MustParseAddrPort("10.0.0.1:12345") || h.Dst != netip.MustParseAddrPort("10.0.0.2:443") {
		t.Fatalf("%+v", h)
	}
	if rest, _ := io.ReadAll(br); string(rest) != "x" {
		t.Fatalf("%q", rest)
	}
}

func TestV2Invalid(t *testing.T) {
	cases := map[string][]byte{
		"bad version": append([]byte(v2Sig), 0x11, 0x11, 0, 0),
		"bad cmd":     v2(0x22, 0x11, nil),
		"short addr":  v2(0x21, 0x11, make([]byte, 11)),
		"short v6":    v2(0x21, 0x21, make([]byte, 35)),
		"bad fam":     v2(0x21, 0x41, make([]byte, 40)),
		"bad proto":   v2(0x21, 0x13, make([]byte, 12)),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadHeader(bufio.NewReader(bytes.NewReader(in))); !errors.Is(err, ErrMalformed) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestReadHeaderShortNonProxyImmediate(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close(); _ = s.Close() }()
	go func() { _, _ = c.Write([]byte("hi")) }() // peer then stays silent

	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(s)
	start := time.Now()
	if _, err := ReadHeader(br); !errors.Is(err, ErrNotProxy) {
		t.Fatalf("err %v, want ErrNotProxy", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("ReadHeader waited for more bytes")
	}
	got, _ := br.Peek(br.Buffered())
	if !strings.HasPrefix(string(got), "hi") || len(got) == 0 {
		t.Fatalf("buffered %q", got)
	}
}

func TestReadHeaderPartialThenTimeout(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close(); _ = s.Close() }()
	go func() { _, _ = c.Write([]byte("PRO")) }()

	_ = s.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := ReadHeader(bufio.NewReader(s))
	if err == nil || errors.Is(err, ErrNotProxy) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err %v, want malformed wrapping a timeout", err)
	}
}

func TestReadHeaderEmptyTimeoutNoData(t *testing.T) {
	c, s := net.Pipe()
	defer func() { _ = c.Close(); _ = s.Close() }()

	_ = s.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	_, err := ReadHeader(bufio.NewReader(s))
	if !errors.Is(err, ErrNoData) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err %v, want ErrNoData wrapping a timeout", err)
	}
}
