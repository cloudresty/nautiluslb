package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
)

// maxProbeBody caps how much of an HTTP probe response body is read.
const maxProbeBody = 64 << 10

// Prober checks one backend address. A nil error means healthy.
type Prober interface {
	Probe(ctx context.Context, addr string) error
}

type tcpProber struct {
	dialer net.Dialer
}

// NewTCPProber returns a Prober that succeeds when a TCP connection to the
// address can be established; the connection is closed immediately.
func NewTCPProber() Prober {
	return &tcpProber{}
}

func (p *tcpProber) Probe(ctx context.Context, addr string) error {

	conn, err := p.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp probe %s: %w", addr, err)
	}

	return conn.Close()

}

type httpProber struct {
	client *http.Client
	path   string
	host   string
	expect []int
}

// NewHTTPProber returns a Prober that issues GET http://addr+path, never
// follows redirects, discards at most 64KB of the body and succeeds when the
// status is in expect (default [200]). A non-empty host overrides the Host
// header.
func NewHTTPProber(path, host string, expect []int) Prober {

	if len(expect) == 0 {
		expect = []int{http.StatusOK}
	}

	return &httpProber{
		client: &http.Client{
			Transport: &http.Transport{DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		path:   path,
		host:   host,
		expect: slices.Clone(expect),
	}

}

func (p *httpProber) Probe(ctx context.Context, addr string) error {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+p.path, nil)
	if err != nil {
		return fmt.Errorf("http probe %s: %w", addr, err)
	}

	if p.host != "" {
		req.Host = p.host
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("http probe %s: %w", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))

	if !slices.Contains(p.expect, resp.StatusCode) {
		return fmt.Errorf("http probe %s: unexpected status %d", addr, resp.StatusCode)
	}

	return nil

}
