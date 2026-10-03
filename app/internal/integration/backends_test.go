//go:build integration

package integration

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/proxyproto"
)

type backendMode int

const (
	// modeID writes "<id>\n" on accept, then echoes.
	modeID backendMode = iota
	// modeProxy parses an inbound PROXY header, answers "v<N> src=<addr>\n"
	// (or "v<N> local\n"), then echoes.
	modeProxy
)

// tcpBackend is a loopback TCP server that identifies itself. It can be
// stopped and restarted on the same port.
type tcpBackend struct {
	t    testing.TB
	id   string
	mode backendMode
	tls  *tls.Config // non-nil: serve TLS
	port int
	addr string

	accepted atomic.Int64

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func newBackend(t testing.TB, id string) *tcpBackend {
	return startBackend(t, id, modeID, nil)
}

func startBackend(t testing.TB, id string, mode backendMode, tc *tls.Config) *tcpBackend {
	t.Helper()
	b := &tcpBackend{t: t, id: id, mode: mode, tls: tc, conns: map[net.Conn]struct{}{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b.port = ln.Addr().(*net.TCPAddr).Port
	b.addr = ln.Addr().String()
	b.serve(ln)
	t.Cleanup(b.stop)
	return b
}

func (b *tcpBackend) serve(ln net.Listener) {
	if b.tls != nil {
		ln = tls.NewListener(ln, b.tls)
	}
	b.mu.Lock()
	b.ln = ln
	b.mu.Unlock()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.accepted.Add(1)
			b.mu.Lock()
			b.conns[c] = struct{}{}
			b.mu.Unlock()
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				defer func() {
					_ = c.Close()
					b.mu.Lock()
					delete(b.conns, c)
					b.mu.Unlock()
				}()
				b.handle(c)
			}()
		}
	}()
}

func (b *tcpBackend) handle(c net.Conn) {
	var r io.Reader = c
	switch b.mode {
	case modeProxy:
		br := bufio.NewReader(c)
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		hdr, err := proxyproto.ReadHeader(br)
		_ = c.SetReadDeadline(time.Time{})
		if err != nil {
			_, _ = fmt.Fprintf(c, "error %v\n", err)
			return
		}
		if hdr.Local {
			_, _ = fmt.Fprintf(c, "v%d local\n", hdr.Version)
		} else {
			_, _ = fmt.Fprintf(c, "v%d src=%s\n", hdr.Version, hdr.Src)
		}
		r = br
	default:
		if _, err := fmt.Fprintf(c, "%s\n", b.id); err != nil {
			return
		}
	}
	_, _ = io.Copy(c, r)
}

// stop closes the listener and every open connection.
func (b *tcpBackend) stop() {
	b.mu.Lock()
	if b.ln != nil {
		_ = b.ln.Close()
		b.ln = nil
	}
	for c := range b.conns {
		_ = c.Close()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// restart listens again on the same address.
func (b *tcpBackend) restart() {
	b.t.Helper()
	var ln net.Listener
	var err error
	for range 200 {
		if ln, err = net.Listen("tcp", b.addr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		b.t.Fatalf("restarting backend %s on %s: %v", b.id, b.addr, err)
	}
	b.serve(ln)
}

// udpBackend echoes datagrams and records the distinct sources it saw.
type udpBackend struct {
	conn net.PacketConn
	port int

	mu      sync.Mutex
	sources map[string]int
}

func newUDPBackend(t testing.TB) *udpBackend {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &udpBackend{conn: c, port: c.LocalAddr().(*net.UDPAddr).Port, sources: map[string]int{}}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			b.mu.Lock()
			b.sources[from.String()]++
			b.mu.Unlock()
			_, _ = c.WriteTo(buf[:n], from)
		}
	}()
	return b
}

func (b *udpBackend) distinctSources() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sources)
}

// selfSigned generates a self-signed certificate valid for host.
func selfSigned(t testing.TB, host string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}
