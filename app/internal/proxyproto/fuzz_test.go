package proxyproto

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
)

func FuzzReadHeader(f *testing.F) {
	addrs := [][2]net.Addr{
		{tcp("192.0.2.1:1"), tcp("192.0.2.2:2")},
		{tcp("[::1]:1"), tcp("[::2]:2")},
		{tcp("[2001:db8::1]:65535"), tcp("[2001:db8::2]:0")},
		{tcp("[::ffff:192.0.2.1]:1"), tcp("[::ffff:192.0.2.9]:2")},
		{tcp("192.0.2.1:1"), tcp("[2001:db8::2]:2")},
		{udp("1.1.1.1:1"), udp("2.2.2.2:2")},
		{udp("[2001:db8::1]:53"), udp("[2001:db8::2]:53")},
		{&net.UnixAddr{Name: "/x", Net: "unix"}, &net.UnixAddr{Name: "/y", Net: "unix"}}, // UNKNOWN / UNSPEC
	}
	for _, v := range []Version{V1, V2} {
		for _, p := range addrs {
			var b bytes.Buffer
			_ = WriteHeader(&b, v, p[0], p[1])
			f.Add(b.Bytes())
			f.Add(append(append([]byte(nil), b.Bytes()...), "GET / HTTP/1.1\r\nHost: x\r\n\r\n"...))
			f.Add(b.Bytes()[:b.Len()/2])
		}
	}
	// hand-written v1 / v2 headers
	f.Add([]byte("PROXY TCP4 192.0.2.1 192.0.2.2 1 2\r\npayload"))
	f.Add([]byte("PROXY TCP6 ::1 ::2 1 2\r\n"))
	f.Add([]byte("PROXY UNKNOWN\r\n"))
	f.Add([]byte("PROXY UNKNOWN ffff::1 ::1 1 2\r\nrest"))
	f.Add([]byte("PROXY TCP4 192.0.2.1 192.0.2.2 99999 2\r\n"))
	f.Add([]byte("PROXY "))
	f.Add(v2(0x20, 0, nil))                                                // LOCAL
	f.Add(append(v2(0x20, 0x11, make([]byte, 12)), "x"...))                // LOCAL with addresses + payload
	f.Add(v2(0x21, 0x31, make([]byte, 216)))                               // AF_UNIX
	f.Add(v2(0x21, 0x11, append(make([]byte, 12), 0x01, 0x00, 0x01, 'a'))) // with TLV
	f.Add(v2(0x21, 0x00, nil))
	f.Add([]byte("GET / HTTP/1.1\r\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, in []byte) {
		br := bufio.NewReader(bytes.NewReader(in))
		h, err := ReadHeader(br)
		rest, _ := io.ReadAll(br)
		if errors.Is(err, ErrNotProxy) {
			if !bytes.Equal(rest, in) {
				t.Fatal("ErrNotProxy consumed bytes")
			}
			return
		}
		if err == nil {
			consumed := len(in) - len(rest)
			if consumed <= 0 || consumed > len(in) {
				t.Fatalf("consumed %d of %d", consumed, len(in))
			}
			if h.Version == V1 && consumed > v1MaxLen {
				t.Fatalf("v1 header of %d bytes exceeds %d", consumed, v1MaxLen)
			}
			if h.Version == V2 && consumed > v2HdrLen+v2MaxLen {
				t.Fatalf("v2 header of %d bytes", consumed)
			}
			if !h.Local && h.Transport != UNKNOWN && (!h.Src.IsValid() || !h.Dst.IsValid()) {
				t.Fatalf("accepted %s header with invalid addresses: %+v", h.Transport, h)
			}
		}
	})
}

func FuzzWriteReadRoundTrip(f *testing.F) {
	f.Add(uint8(2), uint8(0), []byte{192, 0, 2, 1}, []byte{198, 51, 100, 2}, uint16(1234), uint16(443), []byte("payload"))
	f.Add(uint8(1), uint8(1), []byte{0x20, 0x01, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, []byte{0x20, 0x01, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}, uint16(0), uint16(65535), []byte(nil))
	f.Add(uint8(2), uint8(2), []byte{1, 1, 1, 1}, []byte{2, 2, 2, 2}, uint16(53), uint16(53), []byte("\r\n"))
	f.Add(uint8(2), uint8(3), []byte{0xff}, []byte{0}, uint16(1), uint16(2), []byte("PROXY "))
	f.Add(uint8(1), uint8(0), []byte{1, 2, 3, 4}, []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, uint16(1), uint16(2), []byte("x"))

	f.Fuzz(func(t *testing.T, ver, kind uint8, a, b []byte, sp, dp uint16, payload []byte) {
		v := V1
		if ver&1 == 1 {
			v = V2
		}
		mk := func(raw []byte) (netip.Addr, bool) {
			switch {
			case len(raw) == 4:
				return netip.AddrFrom4([4]byte(raw)), true
			case len(raw) == 16:
				return netip.AddrFrom16([16]byte(raw)), true
			}
			return netip.Addr{}, false
		}
		sa, ok1 := mk(a)
		da, ok2 := mk(b)
		if !ok1 || !ok2 {
			return
		}
		isUDP := kind&1 == 1
		var src, dst net.Addr
		if isUDP {
			src, dst = net.UDPAddrFromAddrPort(netip.AddrPortFrom(sa, sp)), net.UDPAddrFromAddrPort(netip.AddrPortFrom(da, dp))
		} else {
			src, dst = net.TCPAddrFromAddrPort(netip.AddrPortFrom(sa, sp)), net.TCPAddrFromAddrPort(netip.AddrPortFrom(da, dp))
		}
		var buf bytes.Buffer
		if err := WriteHeader(&buf, v, src, dst); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		hdrLen := buf.Len()
		buf.Write(payload)
		br := bufio.NewReader(&buf)
		h, err := ReadHeader(br)
		if err != nil {
			t.Fatalf("ReadHeader of own output %q: %v", buf.Bytes()[:hdrLen], err)
		}
		rest, _ := io.ReadAll(br)
		if !bytes.Equal(rest, payload) {
			t.Fatalf("payload %q, want %q", rest, payload)
		}
		if h.Version != v {
			t.Fatalf("version %d want %d", h.Version, v)
		}
		if v == V1 && isUDP {
			if h.Transport != UNKNOWN {
				t.Fatalf("v1 UDP: transport %q", h.Transport)
			}
			return
		}
		wantSrc := netip.AddrPortFrom(sa.Unmap(), sp)
		wantDst := netip.AddrPortFrom(da.Unmap(), dp)
		// Mixed families are widened to v6 on the wire; compare unmapped.
		unmap := func(ap netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()) }
		if h.Local || unmap(h.Src) != wantSrc || unmap(h.Dst) != wantDst {
			t.Fatalf("round trip: got %+v, want %v -> %v", h, wantSrc, wantDst)
		}
	})
}
