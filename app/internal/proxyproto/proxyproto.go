// Package proxyproto reads and writes HAProxy PROXY protocol v1 and v2 headers.
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Version is the PROXY protocol version.
type Version uint8

const (
	V1 Version = 1
	V2 Version = 2
)

// Transport values carried in Header.Transport.
const (
	TCP4    = "TCP4"
	TCP6    = "TCP6"
	UDP4    = "UDP4"
	UDP6    = "UDP6"
	UNKNOWN = "UNKNOWN"
)

const (
	v1MaxLen   = 107
	v2MaxLen   = 1024
	v2HdrLen   = 16
	v2SigLen   = 12
	v1Sig      = "PROXY "
	v2Sig      = "\r\n\r\n\x00\r\nQUIT\n"
	famUnspec  = 0x0
	famInet    = 0x1
	famInet6   = 0x2
	famUnix    = 0x3
	protoStrm  = 0x1
	protoDgram = 0x2
)

var (
	// ErrNotProxy means the first bytes are not a PROXY signature; nothing was consumed.
	ErrNotProxy = errors.New("proxyproto: no PROXY header")
	// ErrNoData means the connection yielded no bytes before the read failed
	// (timeout, EOF or error); it wraps the underlying read error. A caller
	// that does not require PROXY treats it as "not proxy".
	ErrNoData = errors.New("proxyproto: no data before header")
	// ErrMalformed means a PROXY signature was present but the header is invalid.
	ErrMalformed = errors.New("proxyproto: malformed header")
)

// Header is a parsed PROXY protocol header.
type Header struct {
	Version   Version
	Transport string
	Src, Dst  netip.AddrPort
	Local     bool
}

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

// endpoint extracts the transport ("TCP"/"UDP") and address from a net.Addr.
func endpoint(a net.Addr) (proto string, ap netip.AddrPort, ok bool) {
	switch v := a.(type) {
	case nil:
		return "", netip.AddrPort{}, false
	case *net.TCPAddr:
		if v == nil {
			return "", netip.AddrPort{}, false
		}
		return "TCP", v.AddrPort(), v.AddrPort().IsValid()
	case *net.UDPAddr:
		if v == nil {
			return "", netip.AddrPort{}, false
		}
		return "UDP", v.AddrPort(), v.AddrPort().IsValid()
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return "", netip.AddrPort{}, false
	}
	return "TCP", ap, true
}

// WriteHeader writes a PROXY header describing a connection from src to dst.
// Non-IP addresses yield "PROXY UNKNOWN" (v1) or a v2 PROXY with AF_UNSPEC.
func WriteHeader(w io.Writer, v Version, src, dst net.Addr) error {
	sp, sap, sok := endpoint(src)
	dp, dap, dok := endpoint(dst)
	ok := sok && dok && sp == dp
	var s, d netip.Addr
	if ok {
		s, d = sap.Addr().WithZone("").Unmap(), dap.Addr().WithZone("").Unmap()
		if s.Is4() != d.Is4() { // mixed families: widen both to v6
			s, d = netip.AddrFrom16(s.As16()), netip.AddrFrom16(d.As16())
		}
	}
	var buf bytes.Buffer
	switch v {
	case V1:
		if !ok || sp != "TCP" {
			buf.WriteString("PROXY UNKNOWN\r\n")
			break
		}
		t := TCP4
		if !s.Is4() {
			t = TCP6
		}
		fmt.Fprintf(&buf, "PROXY %s %s %s %d %d\r\n", t, s, d, sap.Port(), dap.Port())
	case V2:
		buf.WriteString(v2Sig)
		buf.WriteByte(0x21)
		if !ok {
			buf.WriteByte(0x00)
			buf.Write([]byte{0, 0})
			break
		}
		var fp byte = famInet << 4
		if !s.Is4() {
			fp = famInet6 << 4
		}
		if sp == "TCP" {
			fp |= protoStrm
		} else {
			fp |= protoDgram
		}
		buf.WriteByte(fp)
		a1, a2 := s.AsSlice(), d.AsSlice()
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(a1)+len(a2)+4))
		buf.Write(a1)
		buf.Write(a2)
		_ = binary.Write(&buf, binary.BigEndian, sap.Port())
		_ = binary.Write(&buf, binary.BigEndian, dap.Port())
	default:
		return fmt.Errorf("proxyproto: unsupported version %d", v)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ReadHeader reads one PROXY header from r. If the stream does not start with
// a PROXY signature it returns ErrNotProxy and consumes nothing. The signature
// is peeked progressively: as soon as the buffered bytes can no longer be the
// start of a v1 or v2 signature the call returns ErrNotProxy without waiting
// for more data. If the read fails before any byte arrived (timeout, EOF,
// server-first protocol) it returns ErrNoData wrapping the read error.
func ReadHeader(r *bufio.Reader) (Header, error) {
	for n := 1; n <= v2SigLen; n++ {
		p, perr := r.Peek(n)
		if len(p) < n {
			if len(p) == 0 {
				if perr == nil {
					perr = io.ErrNoProgress
				}
				return Header{}, fmt.Errorf("%w: %w", ErrNoData, perr)
			}
			// Partial bytes that are still a signature prefix: truncated header.
			if perr == nil {
				perr = io.ErrNoProgress
			}
			return Header{}, fmt.Errorf("%w: truncated signature: %w", ErrMalformed, perr)
		}
		switch {
		case n == len(v1Sig) && string(p) == v1Sig:
			return readV1(r)
		case n == v2SigLen && string(p) == v2Sig:
			return readV2(r)
		case !strings.HasPrefix(v2Sig, string(p)) && !strings.HasPrefix(v1Sig, string(p)):
			return Header{}, ErrNotProxy
		}
	}
	return Header{}, ErrNotProxy
}

func readV1(r *bufio.Reader) (Header, error) {
	line := make([]byte, 0, v1MaxLen)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return Header{}, malformed("truncated v1 header")
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
		if len(line) >= v1MaxLen {
			return Header{}, malformed("v1 header too long")
		}
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return Header{}, malformed("v1 header missing CRLF")
	}
	s := string(line[:len(line)-2])
	if strings.ContainsAny(s, "\r\n") {
		return Header{}, malformed("stray CR/LF in v1 header")
	}
	f := strings.Split(s, " ")
	if f[0] != "PROXY" || len(f) < 2 {
		return Header{}, malformed("bad v1 prefix")
	}
	h := Header{Version: V1, Transport: f[1]}
	switch f[1] {
	case UNKNOWN:
		h.Local = false
		return h, nil
	case TCP4, TCP6:
	default:
		return Header{}, malformed("bad v1 protocol %q", f[1])
	}
	if len(f) != 6 {
		return Header{}, malformed("v1 needs 6 fields, got %d", len(f))
	}
	want4 := f[1] == TCP4
	src, err := parseIP(f[2], want4)
	if err != nil {
		return Header{}, err
	}
	dst, err := parseIP(f[3], want4)
	if err != nil {
		return Header{}, err
	}
	sp, err := parsePort(f[4])
	if err != nil {
		return Header{}, err
	}
	dp, err := parsePort(f[5])
	if err != nil {
		return Header{}, err
	}
	h.Src, h.Dst = netip.AddrPortFrom(src, sp), netip.AddrPortFrom(dst, dp)
	return h, nil
}

func parseIP(s string, want4 bool) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, malformed("bad address %q", s)
	}
	if want4 != a.Is4() {
		return netip.Addr{}, malformed("address %q does not match family", s)
	}
	return a, nil
}

func parsePort(s string) (uint16, error) {
	if s == "" || len(s) > 5 || (len(s) > 1 && s[0] == '0') {
		return 0, malformed("bad port %q", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, malformed("bad port %q", s)
		}
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, malformed("bad port %q", s)
	}
	return uint16(n), nil
}

func readV2(r *bufio.Reader) (Header, error) {
	var hdr [v2HdrLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Header{}, malformed("truncated v2 header")
	}
	if hdr[12]>>4 != 2 {
		return Header{}, malformed("bad v2 version %d", hdr[12]>>4)
	}
	cmd := hdr[12] & 0x0f
	if cmd > 1 {
		return Header{}, malformed("bad v2 command %d", cmd)
	}
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if n > v2MaxLen {
		return Header{}, malformed("v2 length %d exceeds cap", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Header{}, malformed("truncated v2 payload")
	}
	h := Header{Version: V2}
	if cmd == 0 {
		h.Local = true
		return h, nil
	}
	fam, proto := hdr[13]>>4, hdr[13]&0x0f
	switch fam {
	case famUnspec:
		if proto != 0 {
			return Header{}, malformed("AF_UNSPEC with protocol %d", proto)
		}
		h.Transport = UNKNOWN
		return h, nil
	case famUnix:
		if proto != protoStrm && proto != protoDgram {
			return Header{}, malformed("bad v2 protocol %d", proto)
		}
		if n < 216 {
			return Header{}, malformed("v2 unix payload too short")
		}
		h.Transport = UNKNOWN
		return h, nil
	case famInet, famInet6:
	default:
		return Header{}, malformed("bad v2 family %d", fam)
	}
	if proto != protoStrm && proto != protoDgram {
		return Header{}, malformed("bad v2 protocol %d", proto)
	}
	al := 4
	if fam == famInet6 {
		al = 16
	}
	if n < 2*al+4 {
		return Header{}, malformed("v2 address block too short")
	}
	src, _ := netip.AddrFromSlice(body[:al])
	dst, _ := netip.AddrFromSlice(body[al : 2*al])
	sp := binary.BigEndian.Uint16(body[2*al:])
	dp := binary.BigEndian.Uint16(body[2*al+2:])
	h.Src, h.Dst = netip.AddrPortFrom(src, sp), netip.AddrPortFrom(dst, dp)
	// Remaining bytes are TLVs: accepted and ignored.
	t := "TCP"
	if proto == protoDgram {
		t = "UDP"
	}
	if fam == famInet {
		h.Transport = t + "4"
	} else {
		h.Transport = t + "6"
	}
	return h, nil
}
