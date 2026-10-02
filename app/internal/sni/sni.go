// Package sni extracts the TLS server name from a ClientHello without
// terminating TLS, and routes server names to named routes.
package sni

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	ErrNotTLS   = errors.New("sni: not a TLS ClientHello")
	ErrTooLarge = errors.New("sni: ClientHello exceeds limit")
	ErrNoSNI    = errors.New("sni: no server_name extension")
)

const maxRecord = 16384

// Peek reads one ClientHello (possibly spanning several TLS records) from r,
// consuming at most max bytes, and returns the lowercased server name and every
// byte consumed (also on error), which the caller must replay.
func Peek(r io.Reader, max int) (serverName string, consumed []byte, err error) {
	var hs []byte
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:1]); err != nil {
			return "", consumed, err
		}
		consumed = append(consumed, hdr[0])
		if hdr[0] != 0x16 {
			return "", consumed, ErrNotTLS
		}
		if n, err := io.ReadFull(r, hdr[1:]); err != nil {
			consumed = append(consumed, hdr[1:1+n]...)
			return "", consumed, err
		}
		consumed = append(consumed, hdr[1:]...)
		if hdr[1] != 3 || hdr[2] < 1 || hdr[2] > 4 {
			return "", consumed, ErrNotTLS
		}
		rl := int(binary.BigEndian.Uint16(hdr[3:5]))
		if rl == 0 || rl > maxRecord {
			return "", consumed, ErrNotTLS
		}
		if len(consumed)+rl > max {
			return "", consumed, ErrTooLarge
		}
		rec := make([]byte, rl)
		n, err := io.ReadFull(r, rec)
		consumed = append(consumed, rec[:n]...)
		if err != nil {
			return "", consumed, err
		}
		hs = append(hs, rec...)
		if len(hs) >= 4 {
			if hs[0] != 1 {
				return "", consumed, ErrNotTLS
			}
			hl := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
			if 4+hl > max {
				return "", consumed, ErrTooLarge
			}
			if len(hs) >= 4+hl {
				name, err := parseClientHello(hs[4 : 4+hl])
				return name, consumed, err
			}
		}
	}
}

type reader struct {
	b   []byte
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() int {
	v := r.take(1)
	if v == nil {
		return 0
	}
	return int(v[0])
}

func (r *reader) u16() int {
	v := r.take(2)
	if v == nil {
		return 0
	}
	return int(binary.BigEndian.Uint16(v))
}

func parseClientHello(b []byte) (string, error) {
	bad := fmt.Errorf("%w: malformed ClientHello", ErrNotTLS)
	r := &reader{b: b}
	r.take(2 + 32)
	r.take(r.u8())  // session id
	r.take(r.u16()) // cipher suites
	r.take(r.u8())  // compression
	if r.bad {
		return "", bad
	}
	if len(r.b) == 0 {
		return "", ErrNoSNI
	}
	ext := &reader{b: r.take(r.u16())}
	if r.bad {
		return "", bad
	}
	for len(ext.b) > 0 {
		typ := ext.u16()
		data := &reader{b: ext.take(ext.u16())}
		if ext.bad {
			return "", bad
		}
		if typ != 0 {
			continue
		}
		list := &reader{b: data.take(data.u16())}
		if data.bad {
			return "", bad
		}
		for len(list.b) > 0 {
			nt := list.u8()
			name := list.take(list.u16())
			if list.bad {
				return "", bad
			}
			if nt == 0 {
				return normalize(string(name))
			}
		}
		return "", ErrNoSNI
	}
	return "", ErrNoSNI
}

func normalize(s string) (string, error) {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || len(s) > 253 {
		return "", ErrNoSNI
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", ErrNoSNI
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", ErrNoSNI
			}
		}
	}
	return s, nil
}
