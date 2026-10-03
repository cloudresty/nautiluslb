package sni

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// firstRecord returns the first complete TLS record in b.
func firstRecord(b []byte) []byte {
	if len(b) >= 5 {
		if n := 5 + int(binary.BigEndian.Uint16(b[3:5])); len(b) >= n {
			return b[:n]
		}
	}
	return b
}

// helloWith captures the first flight of a crypto/tls client using cfg.
func helloWith(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	cli, srv := net.Pipe()
	defer func() { _ = srv.Close() }()
	rc := &recConn{Conn: cli}
	go func() { _ = tls.Client(rc, cfg).Handshake() }()
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
	return append([]byte(nil), buf...)
}

func selfSigned(t testing.TB) tls.Certificate {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"*.example.com", "example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// resumedHello performs a full handshake so the client caches a session
// ticket, then captures the client's next hello (carrying the ticket / PSK).
func resumedHello(t testing.TB, version uint16, name string) []byte {
	t.Helper()
	srvCfg := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: version, MaxVersion: version}
	cache := tls.NewLRUClientSessionCache(4)
	cliCfg := &tls.Config{ServerName: name, InsecureSkipVerify: true, ClientSessionCache: cache, MinVersion: version, MaxVersion: version}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s, err := l.Accept()
		if err != nil {
			return
		}
		_ = s.SetDeadline(time.Now().Add(5 * time.Second))
		sc := tls.Server(s, srvCfg)
		if sc.Handshake() == nil {
			_, _ = sc.Write([]byte("x"))
		}
		_ = sc.Close()
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cc := tls.Client(c, cliCfg)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := cc.Handshake(); err != nil {
		t.Fatal(err)
	}
	_, _ = cc.Read(make([]byte, 1)) // process the post-handshake ticket
	_ = cc.Close()
	<-done
	return firstRecord(helloWith(t, cliCfg))
}

func FuzzPeek(f *testing.F) {
	names := []string{"example.com", "", "a.b.c.example.org", "Example.COM.", "xn--bcher-kva.example", "bad_name.example.com", strings.Repeat("a", 63) + ".com"}
	for _, n := range names {
		h := hello(f, n)
		f.Add(h, 16384)
		f.Add(splitRecords(h, 3), 4096)
		f.Add(splitRecords(h, 7), 16384)
		f.Add(h[:len(h)/2], 16384)
	}
	cfgs := map[string]*tls.Config{
		"tls12":    {ServerName: "tls12.example.com", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12},
		"tls13":    {ServerName: "tls13.example.com", InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
		"alpn":     {ServerName: "alpn.example.com", InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}},
		"ip":       {ServerName: "127.0.0.1", InsecureSkipVerify: true},
		"noserver": {InsecureSkipVerify: true},
	}
	for _, c := range cfgs {
		h := helloWith(f, c)
		f.Add(h, 16384)
		f.Add(splitRecords(h, 2), 16384)
		f.Add(append(append([]byte(nil), h...), "APPDATA"...), 16384)
	}
	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		h := resumedHello(f, v, "ticket.example.com")
		f.Add(h, 16384)
		f.Add(splitRecords(h, 4), 16384)
	}
	f.Add([]byte("GET / HTTP/1.1\r\n"), 100)
	f.Add([]byte{0x16, 0x03, 0x01, 0xff, 0xff}, 100)
	f.Add([]byte{}, 0)
	f.Fuzz(func(t *testing.T, in []byte, max int) {
		if max < 0 || max > 1<<16 {
			return
		}
		name, consumed, err := Peek(bytes.NewReader(in), max)
		if max >= 5 && len(consumed) > max {
			t.Fatalf("consumed %d > max %d", len(consumed), max)
		}
		if len(consumed) > len(in) {
			t.Fatal("consumed more than input")
		}
		if !bytes.HasPrefix(in, consumed) {
			t.Fatal("consumed is not a prefix of the input")
		}
		if err == nil && name == "" {
			t.Fatal("nil error with empty server name")
		}
	})
}

func FuzzRouterMatch(f *testing.F) {
	f.Add("a.example.com", "*.example.com", "a.example.com")
	f.Add("*.Example.com", "exact.example.com", "x.EXAMPLE.com.")
	f.Add("example.com", "*.example.com", "a.b.example.com")
	f.Add("", "*", "")
	f.Add("*.", "**.x", ".")
	f.Add(" a.com ", "a.com.", "A.COM")
	f.Fuzz(func(t *testing.T, h1, h2, name string) {
		r, err := NewRouter([]RouteHosts{{"a", []string{h1}}, {"b", []string{h2}}}, "dflt")
		if err != nil {
			return
		}
		got, ok := r.Match(name)
		if !ok || (got != "a" && got != "b" && got != "dflt") {
			t.Fatalf("Match(%q) = %q, %v", name, got, ok)
		}
		if g2, ok2 := r.Match(name); g2 != got || ok2 != ok {
			t.Fatal("Match is not deterministic")
		}

		// Single route, no default: a configured exact host matches itself.
		r1, err := NewRouter([]RouteHosts{{"a", []string{h1}}}, "")
		if err != nil {
			t.Fatalf("one host accepted in a pair but rejected alone: %v", err)
		}
		norm := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h1), "."))
		if !strings.Contains(norm, "*") && norm != "" {
			if got, ok := r1.Match(norm); !ok || got != "a" {
				t.Fatalf("exact host %q (from %q) did not match itself: %q %v", norm, h1, got, ok)
			}
		}
		if _, ok := r1.Match(name); ok && name == "" {
			t.Fatal("empty server name matched without a default route")
		}
	})
}
