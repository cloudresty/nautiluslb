package tcpproxy

import (
	"bufio"
	"context"
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
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/config"
	"github.com/cloudresty/nautiluslb/internal/limits"
	"github.com/cloudresty/nautiluslb/internal/pipe"
	"github.com/cloudresty/nautiluslb/internal/proxyproto"
)

// holdOpen dials addr and returns the connection after a round trip proved
// it is proxied (the echo backend answered).
func holdOpen(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write([]byte("p")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("held connection was not proxied: %v", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c
}

// expectRejected asserts a fresh connection is closed without being proxied.
func expectRejected(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("x"))
	tpWaitClosed(t, c, 3*time.Second)
}

type noReadConn struct {
	net.Conn
	t *testing.T
}

func (c noReadConn) Read([]byte) (int, error) {
	c.t.Error("ACL-rejected connection was read")
	return 0, io.EOF
}

func TestACLRejectsBeforeRead(t *testing.T) {
	cfg := tpConfig()
	cfg.Access.Deny = []string{"127.0.0.0/8"}
	dials := 0
	p := newFakePool("test", nb(t, "192.0.2.1:80"))
	h := newTP(t, cfg, poolsOf(p), func(o *Options) {
		o.Dial = func(context.Context, string, string) (net.Conn, error) { dials++; return nil, io.EOF }
	})

	// A real peer so the ACL sees 127.0.0.1; reading it would fail the test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			defer func() { _ = c.Close() }()
			_, _ = c.Write([]byte("hello"))
			time.Sleep(200 * time.Millisecond)
		}
	}()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	h.l.handle(noReadConn{srv, t})

	if dials != 0 || p.picks != 0 {
		t.Errorf("rejected connection reached the backend path (dials=%d picks=%d)", dials, p.picks)
	}
	if got := h.rec.get(func(r *fakeRec) int { return r.rejected["acl"] }); got != 1 {
		t.Errorf("rejected{acl} = %d, want 1", got)
	}
	if got := h.rec.get(func(r *fakeRec) int { return r.accepted }); got != 0 {
		t.Errorf("accepted = %d, want 0 (ACL runs before accept accounting)", got)
	}
	if recs := h.log.all(); len(recs) != 1 || recs[0].Result != accesslog.ResultACL {
		t.Errorf("records = %+v", recs)
	}
}

func TestACLAllowListAdmitsListedSource(t *testing.T) {
	cfg := tpConfig()
	cfg.Access.Allow = []string{"127.0.0.1/32"}
	h := startTP(t, cfg, poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
	if got := tpRoundTrip(t, h.addr, []byte("ok")); string(got) != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestLimitsOrderAndRelease(t *testing.T) {
	newH := func(t *testing.T, global *limits.Gate, listenerMax, perSource int) *harness {
		cfg := tpConfig()
		cfg.Limits.MaxConnections = listenerMax
		cfg.Limits.MaxConnectionsPerSource = perSource
		return startTP(t, cfg, poolsOf(newFakePool("test", nb(t, tpEcho(t)))), func(o *Options) { o.Global = global })
	}
	rejected := func(h *harness, reason string) int {
		return h.rec.get(func(r *fakeRec) int { return r.rejected[reason] })
	}

	t.Run("global first", func(t *testing.T) {
		global := limits.NewGate(1)
		global.TryAcquire() // another listener holds the only global slot
		h := newH(t, global, 1, 1)
		expectRejected(t, h.addr)
		eventually(t, 3*time.Second, func() bool { return rejected(h, "limit_global") == 1 }, "limit_global")
		if h.l.gate.Active() != 0 {
			t.Errorf("listener gate taken although the global gate refused: %d", h.l.gate.Active())
		}
	})

	t.Run("listener second", func(t *testing.T) {
		global := limits.NewGate(10)
		h := newH(t, global, 1, 0)
		holdOpen(t, h.addr)
		expectRejected(t, h.addr)
		eventually(t, 3*time.Second, func() bool { return rejected(h, "limit_listener") == 1 }, "limit_listener")
		if global.Active() != 1 {
			t.Errorf("global slot of the refused connection not released: %d", global.Active())
		}
	})

	t.Run("source third", func(t *testing.T) {
		global := limits.NewGate(10)
		h := newH(t, global, 10, 1)
		holdOpen(t, h.addr)
		expectRejected(t, h.addr)
		eventually(t, 3*time.Second, func() bool { return rejected(h, "limit_source") == 1 }, "limit_source")
		if global.Active() != 1 || h.l.gate.Active() != 1 {
			t.Errorf("slots of the refused connection not released: global=%d listener=%d", global.Active(), h.l.gate.Active())
		}
	})

	t.Run("released on close", func(t *testing.T) {
		global := limits.NewGate(1)
		h := newH(t, global, 1, 1)
		for range 3 { // each round trip must find every slot free again
			if got := tpRoundTrip(t, h.addr, []byte("x")); string(got) != "x" {
				t.Fatalf("got %q", got)
			}
			eventually(t, 3*time.Second, func() bool {
				return global.Active() == 0 && h.l.gate.Active() == 0 && h.l.Active() == 0
			}, "slots released")
		}
	})
}

func TestPerSourceLimit(t *testing.T) {
	cfg := tpConfig()
	cfg.Limits.MaxConnectionsPerSource = 2
	h := startTP(t, cfg, poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)

	a := holdOpen(t, h.addr)
	holdOpen(t, h.addr)
	expectRejected(t, h.addr)

	_ = a.Close()
	eventually(t, 3*time.Second, func() bool { return h.l.Active() == 1 }, "first connection to close")
	if got := tpRoundTrip(t, h.addr, []byte("again")); string(got) != "again" {
		t.Fatalf("connection after a slot freed: got %q", got)
	}
}

func proxyCfg(trusted string, required bool) config.Configuration {
	cfg := tpConfig()
	cfg.ProxyProtocol.In = config.ProxyProtocolIn{TrustedCIDRs: []string{trusted}, Required: required}
	return cfg
}

func TestInboundProxyTrustedOnly(t *testing.T) {
	hdr := "PROXY TCP4 1.2.3.4 5.6.7.8 1111 2222\r\n"

	t.Run("untrusted peer is forwarded verbatim", func(t *testing.T) {
		h := startTP(t, proxyCfg("10.0.0.0/8", false), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
		if got := tpRoundTrip(t, h.addr, []byte(hdr+"hello")); string(got) != hdr+"hello" {
			t.Fatalf("backend saw %q, want the PROXY bytes untouched", got)
		}
		if recs := h.log.waitN(t, 1); recs[0].ProxySrc != "" {
			t.Errorf("untrusted peer's header was parsed: %q", recs[0].ProxySrc)
		}
	})

	t.Run("trusted peer is parsed and stripped", func(t *testing.T) {
		h := startTP(t, proxyCfg("127.0.0.0/8", false), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
		if got := tpRoundTrip(t, h.addr, []byte(hdr+"hello")); string(got) != "hello" {
			t.Fatalf("backend saw %q, want hello", got)
		}
		if recs := h.log.waitN(t, 1); recs[0].ProxySrc != "1.2.3.4:1111" {
			t.Errorf("ProxySrc = %q", recs[0].ProxySrc)
		}
	})
}

func TestInboundProxyRequired(t *testing.T) {
	dialed := 0
	var real net.Dialer
	h := startTP(t, proxyCfg("127.0.0.0/8", true), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), func(o *Options) {
		o.Dial = func(ctx context.Context, n, a string) (net.Conn, error) { dialed++; return real.DialContext(ctx, n, a) }
	})
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	tpWaitClosed(t, c, 3*time.Second)

	recs := h.log.waitN(t, 1)
	if recs[0].Result != accesslog.ResultProxyHeader || dialed != 0 {
		t.Errorf("result=%q dialed=%d", recs[0].Result, dialed)
	}
	if got := h.rec.get(func(r *fakeRec) int { return r.rejected["proxy_header"] }); got != 1 {
		t.Errorf("rejected{proxy_header} = %d", got)
	}
}

func TestInboundProxyMalformedRejected(t *testing.T) {
	h := startTP(t, proxyCfg("127.0.0.0/8", false), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("PROXY TCP4 not-an-ip 5.6.7.8 1 2\r\n"))
	tpWaitClosed(t, c, 3*time.Second)
	if recs := h.log.waitN(t, 1); recs[0].Result != accesslog.ResultProxyHeader {
		t.Errorf("result = %q", recs[0].Result)
	}
}

func TestInboundProxyShortNonProxyPayload(t *testing.T) {
	h := startTP(t, proxyCfg("127.0.0.0/8", false), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
	if got := tpRoundTrip(t, h.addr, []byte("hi")); string(got) != "hi" {
		t.Fatalf("got %q, want hi", got)
	}
}

func testCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsBackend terminates TLS for name and answers every connection with tag.
func tlsBackend(t *testing.T, name, tag string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{testCert(t, name)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				if tc, ok := c.(*tls.Conn); ok && tc.Handshake() != nil {
					return
				}
				_, _ = c.Write([]byte(tag))
			}()
		}
	}()
	return ln.Addr().String()
}

func tlsCfg(peek time.Duration) config.Configuration {
	cfg := tpConfig()
	cfg.Protocol = config.ProtocolTLS
	cfg.TLS = &config.TLS{
		PeekTimeout:    config.Duration(peek),
		MaxClientHello: 16384,
		Routes: []config.Route{
			{Name: "a", Hosts: []string{"a.example.com"}},
			{Name: "b", Hosts: []string{"*.b.example.com"}},
		},
	}
	return cfg
}

func tlsGet(t *testing.T, addr, serverName string) (string, error) {
	t.Helper()
	d := &net.Dialer{Timeout: 3 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := io.ReadAll(c)
	return string(b), err
}

func TestSNIRoutes(t *testing.T) {
	pa := newFakePool("test/a", nb(t, tlsBackend(t, "a.example.com", "A")))
	pb := newFakePool("test/b", nb(t, tlsBackend(t, "x.b.example.com", "B")))
	h := startTP(t, tlsCfg(5*time.Second), map[string]Pool{pa.key: pa, pb.key: pb}, nil)

	// A completed handshake proves the backend saw the entire ClientHello.
	for _, tc := range []struct{ sni, want string }{{"a.example.com", "A"}, {"x.b.example.com", "B"}, {"a.example.com", "A"}} {
		got, err := tlsGet(t, h.addr, tc.sni)
		if err != nil || got != tc.want {
			t.Fatalf("sni %s: got %q err %v, want %q", tc.sni, got, err, tc.want)
		}
	}
	recs := h.log.waitN(t, 3)
	// Result is not asserted: the backend closes first, so the upstream CloseWrite
	// can race (ENOTCONN on linux) and the pipe reports backend_reset.
	if recs[0].Pool == "" || recs[0].SNI == "" {
		t.Errorf("record = %+v", recs[0])
	}
}

func TestSNINoRoute(t *testing.T) {
	pa := newFakePool("test/a", nb(t, tlsBackend(t, "a.example.com", "A")))
	h := startTP(t, tlsCfg(5*time.Second), map[string]Pool{pa.key: pa}, nil)

	if _, err := tlsGet(t, h.addr, "unknown.example.org"); err == nil {
		t.Fatal("handshake to an unrouted name succeeded")
	}
	recs := h.log.waitN(t, 1)
	if recs[0].Result != accesslog.ResultSNINoRoute {
		t.Errorf("result = %q, want sni_no_route", recs[0].Result)
	}
	if got := h.rec.get(func(r *fakeRec) int { return r.rejected["sni_no_route"] }); got != 1 {
		t.Errorf("rejected{sni_no_route} = %d", got)
	}
}

func TestSNINotTLSIsSNIError(t *testing.T) {
	h := startTP(t, tlsCfg(5*time.Second), map[string]Pool{}, nil)
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: a.example.com\r\n\r\n"))
	tpWaitClosed(t, c, 3*time.Second)
	if recs := h.log.waitN(t, 1); recs[0].Result != accesslog.ResultSNIError {
		t.Errorf("result = %q, want sni_error", recs[0].Result)
	}
}

func TestSNIPeekTimeout(t *testing.T) {
	h := startTP(t, tlsCfg(100*time.Millisecond), map[string]Pool{}, nil)
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	start := time.Now()
	tpWaitClosed(t, c, 3*time.Second) // sends nothing
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("peek timeout took %v", el)
	}
	if recs := h.log.waitN(t, 1); recs[0].Result != accesslog.ResultSNIError {
		t.Errorf("result = %q, want sni_error", recs[0].Result)
	}
}

// R-B: PROXY in and SNI share one reader; bytes buffered past the hello (and
// past the PROXY header) must reach the backend.
func TestSNIWithInboundProxyReplaysLeftovers(t *testing.T) {
	cfg := tlsCfg(5 * time.Second)
	cfg.ProxyProtocol.In = config.ProxyProtocolIn{TrustedCIDRs: []string{"127.0.0.0/8"}}
	got := make(chan []byte, 1)
	addr := tpServe(t, func(c net.Conn) {
		// Collect everything until the proxy goes quiet.
		var all []byte
		buf := make([]byte, 4096)
		for {
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := c.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- all
	})
	pa := newFakePool("test/a", nb(t, addr))
	h := startTP(t, cfg, map[string]Pool{pa.key: pa}, nil)

	// Capture a real ClientHello, then send PROXY + hello + trailing bytes at once.
	var hello []byte
	cc, sc := net.Pipe()
	go func() {
		_ = tls.Client(cc, &tls.Config{ServerName: "a.example.com", InsecureSkipVerify: true}).Handshake()
	}()
	buf := make([]byte, 4096)
	n, _ := sc.Read(buf)
	hello = append(hello, buf[:n]...)
	_ = sc.Close()
	_ = cc.Close()

	payload := append(append([]byte("PROXY TCP4 9.9.9.9 8.8.8.8 1 2\r\n"), hello...), []byte("TRAILING-APP-DATA")...)
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write(payload)

	select {
	case b := <-got:
		want := append(append([]byte(nil), hello...), []byte("TRAILING-APP-DATA")...)
		if string(b) != string(want) {
			t.Fatalf("backend saw %d bytes, want %d (hello + trailing data)", len(b), len(want))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend saw nothing")
	}
}

func TestOutboundProxyV1V2(t *testing.T) {
	for _, ver := range []string{"v1", "v2"} {
		t.Run(ver, func(t *testing.T) {
			type seen struct {
				hdr  proxyproto.Header
				rest string
				err  error
			}
			ch := make(chan seen, 1)
			addr := tpServe(t, func(c net.Conn) {
				br := bufio.NewReader(c)
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				hdr, err := proxyproto.ReadHeader(br)
				rest, _ := io.ReadAll(br)
				ch <- seen{hdr, string(rest), err}
			})
			cfg := tpConfig()
			cfg.ProxyProtocol.Out = ver
			h := startTP(t, cfg, poolsOf(newFakePool("test", nb(t, addr))), nil)

			c, err := net.Dial("tcp", h.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			want := c.LocalAddr().(*net.TCPAddr).AddrPort()
			_, _ = c.Write([]byte("payload"))
			_ = c.(*net.TCPConn).CloseWrite()

			select {
			case s := <-ch:
				if s.err != nil {
					t.Fatalf("ReadHeader: %v", s.err)
				}
				if s.hdr.Src != want {
					t.Errorf("header src = %v, want %v", s.hdr.Src, want)
				}
				if s.rest != "payload" {
					t.Errorf("payload after header = %q", s.rest)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("backend saw nothing")
			}
		})
	}
}

func TestOutboundProxyUsesInboundSource(t *testing.T) {
	ch := make(chan netip.AddrPort, 1)
	addr := tpServe(t, func(c net.Conn) {
		hdr, err := proxyproto.ReadHeader(bufio.NewReader(c))
		if err == nil {
			ch <- hdr.Src
		}
	})
	cfg := proxyCfg("127.0.0.0/8", false)
	cfg.ProxyProtocol.Out = "v2"
	h := startTP(t, cfg, poolsOf(newFakePool("test", nb(t, addr))), nil)

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("PROXY TCP4 1.2.3.4 5.6.7.8 1111 2222\r\nx"))
	select {
	case src := <-ch:
		if src.String() != "1.2.3.4:1111" {
			t.Errorf("forwarded src = %v", src)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend saw no header")
	}
}

func dialErr(err error) func(context.Context, string, string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) { return nil, err }
}

func TestLocalDialErrorDoesNotEject(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.EADDRNOTAVAIL} {
		t.Run(errno.Error(), func(t *testing.T) {
			b := nb(t, "192.0.2.1:80")
			p := newFakePool("test", b)
			err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("socket", errno)}
			h := newTP(t, tpConfig(), poolsOf(p), func(o *Options) { o.Dial = dialErr(err) })

			client, done := runHandle(h.l)
			defer func() { _ = client.Close() }()
			waitDone(t, done, 3*time.Second, "handle")

			if p.ejections() != 0 || !b.IsHealthy() {
				t.Errorf("local error ejected the backend (ejections=%d healthy=%v)", p.ejections(), b.IsHealthy())
			}
			if got := h.rec.get(func(r *fakeRec) int { return r.dials["local"] }); got == 0 {
				t.Errorf("BackendDial{local} not recorded: %v", h.rec.dials)
			}
		})
	}
}

func TestRefusedDialEjects(t *testing.T) {
	b := nb(t, tpRefusing(t))
	p := newFakePool("test", b)
	h := startTP(t, tpConfig(), poolsOf(p), nil)

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tpWaitClosed(t, c, 3*time.Second)

	if p.ejections() != 1 || b.IsHealthy() {
		t.Errorf("refused dial did not eject (ejections=%d healthy=%v)", p.ejections(), b.IsHealthy())
	}
	if got := h.rec.get(func(r *fakeRec) int { return r.dials["refused"] }); got != 1 {
		t.Errorf("BackendDial{refused} = %d", got)
	}
}

func TestDrainWaitsThenForces(t *testing.T) {
	t.Run("waits for a connection that finishes", func(t *testing.T) {
		h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
		c := holdOpen(t, h.addr)

		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = c.(*net.TCPConn).CloseWrite()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if forced := h.l.Drain(ctx); forced != 0 {
			t.Errorf("forced = %d, want 0", forced)
		}
		if el := time.Since(start); el < 100*time.Millisecond {
			t.Errorf("Drain returned after %v, before the connection finished", el)
		}
		if _, err := net.DialTimeout("tcp", h.addr, time.Second); err == nil {
			t.Error("listener still accepting after Drain")
		}
	})

	t.Run("forces a connection that never ends", func(t *testing.T) {
		h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
		c := holdOpen(t, h.addr)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if forced := h.l.Drain(ctx); forced != 1 {
			t.Errorf("forced = %d, want 1", forced)
		}
		if h.l.Active() != 0 {
			t.Errorf("Active = %d after Drain", h.l.Active())
		}
		tpWaitClosed(t, c, 3*time.Second)
	})
}

func TestUpdateInPlaceKeepsConnections(t *testing.T) {
	oldPool := newFakePool("test", nb(t, tpEcho(t)))
	h := startTP(t, tpConfig(), poolsOf(oldPool), nil)
	held := holdOpen(t, h.addr)

	newPool := newFakePool("test", nb(t, tpServe2(t, "NEW")))
	cfg := tpConfig()
	cfg.Access.Deny = []string{"10.0.0.0/8"} // does not match loopback
	cfg.Limits.MaxConnectionsPerSource = 5
	if err := h.l.Update(cfg, poolsOf(newPool)); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// The in-flight connection still talks to the OLD backend.
	if _, err := held.Write([]byte("z")); err != nil {
		t.Fatalf("write on held connection: %v", err)
	}
	buf := make([]byte, 1)
	_ = held.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(held, buf); err != nil || buf[0] != 'z' {
		t.Fatalf("held connection broken by Update: %q %v", buf, err)
	}

	// New connections use the new pool.
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 3)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "NEW" {
		t.Fatalf("new connection got %q, %v; want NEW", b, err)
	}

	// And the new ACL applies to new connections only.
	cfg.Access.Deny = []string{"127.0.0.0/8"}
	if err := h.l.Update(cfg, poolsOf(newPool)); err != nil {
		t.Fatal(err)
	}
	expectRejected(t, h.addr)
	if h.l.Active() < 1 {
		t.Errorf("held connection no longer registered: Active=%d", h.l.Active())
	}
	if _, err := held.Write([]byte("y")); err != nil {
		t.Errorf("held connection dropped by a stricter ACL: %v", err)
	}
}

// tpServe2 greets with tag then holds the connection until the peer closes.
func tpServe2(t *testing.T, tag string) string {
	return tpServe(t, func(c net.Conn) {
		_, _ = c.Write([]byte(tag))
		_, _ = io.Copy(io.Discard, c)
	})
}

func TestUpdateAddressChangedRejected(t *testing.T) {
	h := newTP(t, tpConfig(), map[string]Pool{}, nil)

	cfg := tpConfig()
	cfg.ListenerAddress = "127.0.0.1:9"
	if err := h.l.Update(cfg, nil); err != ErrAddressChanged {
		t.Errorf("address change: err = %v, want ErrAddressChanged", err)
	}
	cfg = tpConfig()
	cfg.Protocol = config.ProtocolTLS
	if err := h.l.Update(cfg, nil); err != ErrAddressChanged {
		t.Errorf("protocol change: err = %v, want ErrAddressChanged", err)
	}
	cfg = tpConfig()
	cfg.Access.Deny = []string{"not-a-cidr"}
	if err := h.l.Update(cfg, nil); err == nil || err == ErrAddressChanged {
		t.Errorf("invalid ACL: err = %v, want a parse error", err)
	}
}

func TestAccessLogRecordPerConnection(t *testing.T) {
	cfg := tpConfig()
	cfg.Name = "test"
	good := nb(t, tpEcho(t))
	h := startTP(t, cfg, poolsOf(newFakePool("test", good)), nil)

	for range 3 {
		tpRoundTrip(t, h.addr, []byte("abcd"))
	}
	h.log.waitN(t, 3)
	time.Sleep(50 * time.Millisecond)
	recs := h.log.all()
	if len(recs) != 3 {
		t.Fatalf("records = %d, want exactly one per connection (3)", len(recs))
	}
	for _, r := range recs {
		if r.Listener != "test" || r.Protocol != "tcp" || r.Result != accesslog.ResultOK ||
			r.Backend != good.Address() || r.BytesIn != 4 || r.BytesOut != 4 ||
			r.Client == "" || r.Local == "" || r.Duration <= 0 {
			t.Errorf("unexpected record %+v", r)
		}
	}
}

func TestMetricsCounters(t *testing.T) {
	cfg := tpConfig()
	cfg.Access.Deny = nil
	good := nb(t, tpEcho(t))
	h := startTP(t, cfg, poolsOf(newFakePool("test", good)), nil)

	tpRoundTrip(t, h.addr, []byte("hello"))
	eventually(t, 3*time.Second, func() bool { return h.rec.get(func(r *fakeRec) int { return r.closed["ok"] }) == 1 }, "ConnClosed")

	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if h.rec.accepted != 1 || h.rec.active != 0 || h.rec.bActive != 0 {
		t.Errorf("accepted=%d active=%d backendActive=%d", h.rec.accepted, h.rec.active, h.rec.bActive)
	}
	if h.rec.dials["ok"] != 1 {
		t.Errorf("dials = %v", h.rec.dials)
	}
	if h.rec.bytesIn != 5 || h.rec.bytesOut != 5 {
		t.Errorf("bytes in/out = %d/%d, want 5/5", h.rec.bytesIn, h.rec.bytesOut)
	}
	if len(h.rec.modes) != 1 {
		t.Errorf("pipe modes = %v", h.rec.modes)
	}
	if len(h.rec.rejected) != 0 {
		t.Errorf("rejected = %v", h.rec.rejected)
	}
}

func TestMetricsRejectedAndDraining(t *testing.T) {
	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
	h.l.draining.Store(true) // as Drain does, while the accept loop is still running
	expectRejected(t, h.addr)
	eventually(t, 3*time.Second, func() bool { return h.rec.get(func(r *fakeRec) int { return r.rejected["draining"] }) == 1 }, "rejected{draining}")
	if recs := h.log.waitN(t, 1); recs[0].Result != accesslog.ResultDraining {
		t.Errorf("result = %q", recs[0].Result)
	}
}

func TestIdleTimeoutResult(t *testing.T) {
	cfg := tpConfig()
	cfg.IdleTimeout = config.Duration(150 * time.Millisecond)
	h := startTP(t, cfg, poolsOf(newFakePool("test", nb(t, tpEcho(t)))), nil)
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tpWaitClosed(t, c, 3*time.Second)
	if recs := h.log.waitN(t, 1); recs[0].Result != accesslog.ResultIdleTimeout {
		t.Errorf("result = %q, want idle_timeout", recs[0].Result)
	}
}

func TestSplicePathWithWatchdog(t *testing.T) {
	wd := pipe.NewWatchdog(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wd.Start(ctx)
	defer wd.Stop()

	h := startTP(t, tpConfig(), poolsOf(newFakePool("test", nb(t, tpEcho(t)))), func(o *Options) { o.Watchdog = wd })
	if got := tpRoundTrip(t, h.addr, []byte("spliced")); string(got) != "spliced" {
		t.Fatalf("got %q", got)
	}
	recs := h.log.waitN(t, 1)
	t.Logf("pipe mode: %s", recs[0].Mode)
	if recs[0].Mode == "" {
		t.Error("access-log record has no pipe mode")
	}
}

func TestResultFor(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("copy: %w", err) }
	tests := []struct {
		name string
		res  pipe.Result
		want string
	}{
		{"clean", pipe.Result{}, accesslog.ResultOK},
		{"idle", pipe.Result{Err: wrap(pipe.ErrIdleTimeout), EndedBy: pipe.EndedByTimeout}, accesslog.ResultIdleTimeout},
		{"stall", pipe.Result{Err: wrap(pipe.ErrWriteStall), EndedBy: pipe.EndedByTimeout}, accesslog.ResultTimeout},
		{"halfclose", pipe.Result{Err: wrap(pipe.ErrHalfCloseIdle), EndedBy: pipe.EndedByTimeout}, accesslog.ResultTimeout},
		{"panic", pipe.Result{Err: wrap(pipe.ErrPanic), EndedBy: pipe.EndedByAbort}, accesslog.ResultPanic},
		{"client", pipe.Result{Err: io.ErrClosedPipe, EndedBy: pipe.EndedByClient}, accesslog.ResultClientReset},
		{"backend", pipe.Result{Err: io.ErrClosedPipe, EndedBy: pipe.EndedByBackend}, accesslog.ResultBackendReset},
		{"abort", pipe.Result{Err: io.ErrClosedPipe, EndedBy: pipe.EndedByAbort}, accesslog.ResultBackendReset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := resultFor(tt.res); got != tt.want {
				t.Errorf("resultFor = %q, want %q", got, tt.want)
			}
		})
	}
}

func BenchmarkProxyThroughput(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	ap := netip.MustParseAddrPort(ln.Addr().String())
	bk := newFakePool("test", nbAddr(ap))
	l, err := New(Options{Config: tpConfig(), Pools: map[string]Pool{"test": bk}})
	if err != nil {
		b.Fatal(err)
	}
	if err := l.Listen(); err != nil {
		b.Fatal(err)
	}
	go l.Serve()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		l.Drain(ctx)
	}()

	c, err := net.Dial("tcp", l.Address())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	buf := make([]byte, 64*1024)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for range b.N {
		if _, err := c.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}
