//go:build integration

package integration

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func tlsConf(name, addr, extraTLS string) string {
	return fmt.Sprintf(`  - name: %s
    protocol: tls
    listenerAddress: %q
    namespaces: [default]
%s    tls:
%s      routes:
        - {name: a, hosts: [a.example.test], backendPortName: https-a}
        - {name: b, hosts: [b.example.test], backendPortName: https-b}
`, name, addr, healthNone, extraTLS)
}

func TestTLSSNIRoutes(t *testing.T) {
	addr := anyAddr
	certA, leafA := selfSigned(t, "a.example.test")
	certB, leafB := selfSigned(t, "b.example.test")
	ba := startBackend(t, "A", modeID, &tls.Config{Certificates: []tls.Certificate{certA}})
	bb := startBackend(t, "B", modeID, &tls.Config{Certificates: []tls.Certificate{certB}})

	e := startEnv(t, doc("2s", tlsConf("edge", addr, "")), nil,
		svc("svc-a", "edge/a", ba.port, corev1.ProtocolTCP, "https-a"),
		svc("svc-b", "edge/b", bb.port, corev1.ProtocolTCP, "https-b"))
	addr = e.addr("edge")
	e.waitPool("edge", "edge/a", 1)
	e.waitPool("edge", "edge/b", 1)

	// handshake does a verified TLS handshake through the balancer for host and
	// returns the backend greeting. Verification against the host's own
	// self-signed certificate proves the backend that answered is the right one.
	handshake := func(host string, leaf *x509.Certificate) (string, error) {
		roots := x509.NewCertPool()
		roots.AddCert(leaf)
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
			&tls.Config{ServerName: host, RootCAs: roots})
		if err != nil {
			return "", err
		}
		defer func() { _ = conn.Close() }()
		c := newClient(conn)
		return c.line()
	}

	for _, tc := range []struct {
		host string
		leaf *x509.Certificate
		want string
	}{
		{"a.example.test", leafA, "A"},
		{"b.example.test", leafB, "B"},
		{"a.example.test", leafA, "A"},
	} {
		got, err := handshake(tc.host, tc.leaf)
		if err != nil || got != tc.want {
			t.Fatalf("SNI %s: backend %q, err %v; want %q", tc.host, got, err, tc.want)
		}
	}

	// No default route: an unknown SNI is rejected before any backend is dialed.
	before := ba.accepted.Load() + bb.accepted.Load()
	if got, err := handshake("c.example.test", leafA); err == nil {
		t.Fatalf("unknown SNI c.example.test got a backend (%q), want a rejected handshake", got)
	}
	eventually(t, "sni_no_route access log record", func() bool {
		r, ok := e.log.find(func(r accesslogRecord) bool { return r.Listener == "edge" && r.Result == "sni_no_route" })
		return ok && r.SNI == "c.example.test"
	})
	e.waitMetric("rejected{sni_no_route}", 1, mp+"connections_rejected_total", "listener", "edge", "reason", "sni_no_route")
	if after := ba.accepted.Load() + bb.accepted.Load(); after != before {
		t.Fatalf("a backend was dialed for the unroutable SNI (accepted %d -> %d)", before, after)
	}
}
