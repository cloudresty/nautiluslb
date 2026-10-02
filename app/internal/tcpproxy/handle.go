package tcpproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime/debug"
	"time"

	"github.com/cloudresty/emit"

	"github.com/cloudresty/nautiluslb/internal/accesslog"
	"github.com/cloudresty/nautiluslb/internal/backend"
	"github.com/cloudresty/nautiluslb/internal/balancer"
	"github.com/cloudresty/nautiluslb/internal/neterr"
	"github.com/cloudresty/nautiluslb/internal/pipe"
	"github.com/cloudresty/nautiluslb/internal/proxyproto"
	"github.com/cloudresty/nautiluslb/internal/sni"
)

// connState is everything one connection acquires; the single deferred
// teardown in handle releases it in reverse order, panic or not.
type connState struct {
	rc     *runtimeConfig
	client net.Conn
	start  time.Time

	proxied bool // reached pipe.Run: ConnClosed applies

	peer      netip.AddrPort // transport peer
	effective netip.AddrPort // PROXY header source when one was accepted
	proxySrc  string
	sni       string
	poolKey   string

	pool     Pool
	b        *backend.Backend
	upstream net.Conn

	gGlobal, gListener, gSource bool
	tc                          *trackedConn

	result, reason, errStr string
	in, out                int64
	mode                   string
}

func addrPortOf(a net.Addr) netip.AddrPort {
	if t, ok := a.(*net.TCPAddr); ok {
		ap := t.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	if a != nil {
		if ap, err := netip.ParseAddrPort(a.String()); err == nil {
			return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		}
	}
	return netip.AddrPort{}
}

func (st *connState) fail(reason, result string) {
	st.reason, st.result = reason, result
}

// rejectEarly closes a connection that never reached handle (draining).
func (l *Listener) rejectEarly(conn net.Conn, reason, result string) {
	st := &connState{client: conn, start: time.Now(), peer: addrPortOf(conn.RemoteAddr())}
	st.fail(reason, result)
	_ = conn.Close()
	l.finish(st)
}

// handle proxies one client connection. It always closes conn, and a panic
// anywhere in it is contained to this connection.
func (l *Listener) handle(conn net.Conn) {
	st := &connState{
		rc:     l.rc.Load(), // once; in-flight connections keep their rules
		client: conn,
		start:  time.Now(),
		result: accesslog.ResultOK,
	}
	st.peer = addrPortOf(conn.RemoteAddr())

	defer func() {
		if r := recover(); r != nil {
			emit.Error.StructuredFields("Recovered panic while handling connection",
				emit.ZString("listener", l.name),
				emit.ZString("panic", fmt.Sprint(r)),
				emit.ZString("stack", string(debug.Stack())))
			st.result, st.errStr = accesslog.ResultPanic, fmt.Sprint(r)
			if !st.proxied {
				st.reason = "panic"
			}
		}
		l.teardown(st)
	}()

	l.process(st)
}

func (l *Listener) process(st *connState) {
	rc, conn, ip := st.rc, st.client, st.peer.Addr()

	// ACL: nothing has been read from the client yet.
	if !rc.acl.Allowed(ip) {
		st.fail("acl", accesslog.ResultACL)
		return
	}

	// Limits: global, listener, per source. Released in reverse by teardown.
	if l.global != nil {
		if !l.global.TryAcquire() {
			st.fail("limit_global", accesslog.ResultLimit)
			return
		}
		st.gGlobal = true
	}
	if !l.gate.TryAcquire() {
		st.fail("limit_listener", accesslog.ResultLimit)
		return
	}
	st.gListener = true
	if !l.perSrc.TryAcquire(ip) {
		st.fail("limit_source", accesslog.ResultLimit)
		return
	}
	st.gSource = true

	st.tc = l.reg.add(conn)
	l.rec.ConnAccepted(l.name)
	l.rec.ConnActive(l.name, 1)
	if l.baseCtx.Err() != nil { // a forced drain started while we were admitting
		st.fail("draining", accesslog.ResultDraining)
		return
	}

	effective := st.peer

	// Inbound PROXY (trusted peers only) and SNI share one buffered reader;
	// whatever it holds past what we consumed is replayed upstream.
	var br *bufio.Reader
	if len(rc.trusted) > 0 && rc.isTrusted(ip) {
		br = bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(proxyReadTimeout))
		hdr, err := proxyproto.ReadHeader(br)
		switch {
		case err == nil:
			if !hdr.Local && hdr.Src.IsValid() {
				effective = netip.AddrPortFrom(hdr.Src.Addr().Unmap(), hdr.Src.Port())
				st.proxySrc = effective.String()
			}
			st.effective = effective
		case errors.Is(err, proxyproto.ErrNotProxy) || errors.Is(err, proxyproto.ErrNoData):
			if rc.proxyRequired {
				st.fail("proxy_header", accesslog.ResultProxyHeader)
				st.errStr = err.Error()
				return
			}
		default:
			st.fail("proxy_header", accesslog.ResultProxyHeader)
			st.errStr = err.Error()
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
	}
	if !st.effective.IsValid() {
		st.effective = effective
	}

	var prefix []byte
	st.poolKey = rc.poolKey

	if rc.tls {
		if br == nil {
			br = bufio.NewReader(conn)
		}
		_ = conn.SetReadDeadline(time.Now().Add(rc.peekTimeout))
		name, consumed, err := sni.Peek(br, rc.maxHello)
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil && !errors.Is(err, sni.ErrNoSNI) {
			st.fail("sni_error", accesslog.ResultSNIError)
			st.errStr = err.Error()
			return
		}
		st.sni = name
		route, ok := rc.router.Match(name)
		if !ok {
			st.fail("sni_no_route", accesslog.ResultSNINoRoute)
			return
		}
		key, ok := rc.routeKeys[route]
		if !ok {
			st.fail("sni_no_route", accesslog.ResultSNINoRoute)
			st.errStr = "route " + route + " has no pool"
			return
		}
		st.poolKey = key
		prefix = consumed
	}
	if br != nil {
		if n := br.Buffered(); n > 0 {
			rest, _ := br.Peek(n)
			prefix = append(prefix, rest...)
		}
	}

	pool := rc.pools[st.poolKey]
	if pool == nil {
		st.fail("no_backend", accesslog.ResultNoBackend)
		st.errStr = "no pool " + st.poolKey
		return
	}
	st.pool = pool

	candidates := pool.Pick(balancer.Key{SourceIP: st.effective.Addr()}, dialBudget)
	if len(candidates) == 0 {
		st.fail("no_backend", accesslog.ResultNoBackend)
		return
	}

	if !l.connectBackend(st, candidates) {
		return
	}
	b, upstream := st.b, st.upstream
	st.tc.upstream.Store(&upstream)

	if rc.proxyOut != 0 {
		dst := conn.LocalAddr()
		src := net.Addr(net.TCPAddrFromAddrPort(st.effective))
		_ = upstream.SetWriteDeadline(time.Now().Add(proxyWriteTimeout))
		err := proxyproto.WriteHeader(upstream, rc.proxyOut, src, dst)
		_ = upstream.SetWriteDeadline(time.Time{})
		if err != nil {
			st.fail("backend_write", accesslog.ResultBackendReset)
			st.errStr = "writing PROXY header: " + err.Error()
			return
		}
	}

	emit.Debug.StructuredFields("Proxying connection",
		emit.ZString("listener", l.name),
		emit.ZString("backend", b.Address()))

	st.proxied = true
	res := pipe.Run(conn, upstream, pipe.Options{
		IdleTimeout:      rc.idleTimeout,
		PrefixToUpstream: prefix,
		Watchdog:         l.wd,
		BufferPool:       copyBuffers,
	})
	st.in, st.out, st.mode = res.BytesIn, res.BytesOut, res.Mode
	if res.Mode != "" {
		l.rec.PipeMode(res.Mode)
	}
	st.result, st.errStr = resultFor(res)
}

// connectBackend dials up to dialBudget distinct candidates. It returns
// false, with st filled in, only when every attempt failed (or none could be
// reserved); the caller then closes the client rather than proxy to nil.
func (l *Listener) connectBackend(st *connState, candidates []*backend.Backend) bool {
	attempted := 0
	for _, b := range candidates {
		if !b.TryAcquire() {
			continue // at its connection cap
		}
		attempted++
		st.b = b // slot held: teardown releases it even if the dial panics

		ctx, cancel := context.WithTimeout(l.baseCtx, st.rc.dialTimeout)
		t0 := time.Now()
		conn, err := l.dial(ctx, "tcp", b.Address())
		cancel()
		d := time.Since(t0)

		if err == nil {
			l.rec.BackendDial(l.name, st.poolKey, b.Address(), "ok", d)
			l.rec.BackendActive(l.name, st.poolKey, b.Address(), 1)
			st.upstream = conn
			return true
		}

		cause := neterr.Classify(err)
		l.rec.BackendDial(l.name, st.poolKey, b.Address(), cause.String(), d)
		// Only evidence against the backend ejects it: local exhaustion
		// (EMFILE and friends) must not empty the pool.
		ejected := false
		if neterr.BackendAttributable(cause) && l.baseCtx.Err() == nil {
			st.pool.Eject(b)
			ejected = true
		}
		b.Release()
		st.b = nil

		emit.Debug.StructuredFields("Failed to connect to backend",
			emit.ZString("listener", l.name),
			emit.ZString("backend", b.Address()),
			emit.ZString("cause", cause.String()),
			emit.ZBool("ejected", ejected),
			emit.ZString("error", err.Error()))
		st.errStr = err.Error()
	}

	if attempted == 0 {
		st.fail("no_backend", accesslog.ResultNoBackend)
		st.errStr = "every candidate backend is at its connection cap"
	} else {
		st.fail("dial_failed", accesslog.ResultDialFailed)
	}
	return false
}

// resultFor maps how the pipe ended onto the fixed access-log vocabulary.
func resultFor(res pipe.Result) (result, errStr string) {
	if res.Err == nil {
		return accesslog.ResultOK, ""
	}
	msg := res.Err.Error()
	switch {
	case errors.Is(res.Err, pipe.ErrPanic):
		return accesslog.ResultPanic, msg
	case errors.Is(res.Err, pipe.ErrIdleTimeout):
		return accesslog.ResultIdleTimeout, msg
	case errors.Is(res.Err, pipe.ErrHalfCloseIdle), errors.Is(res.Err, pipe.ErrWriteStall),
		errors.Is(res.Err, os.ErrDeadlineExceeded), res.EndedBy == pipe.EndedByTimeout:
		return accesslog.ResultTimeout, msg
	case res.EndedBy == pipe.EndedByClient:
		return accesslog.ResultClientReset, msg
	default: // backend, or an abort whose side is unknown
		return accesslog.ResultBackendReset, msg
	}
}

// teardown closes and releases everything the connection acquired, in
// reverse acquisition order, then reports it.
func (l *Listener) teardown(st *connState) {
	_ = st.client.Close()
	if st.upstream != nil {
		_ = st.upstream.Close()
	}
	if st.b != nil {
		if st.upstream != nil {
			l.rec.BackendActive(l.name, st.poolKey, st.b.Address(), -1)
		}
		st.b.Release()
	}
	if st.tc != nil {
		l.reg.remove(st.tc)
		l.rec.ConnActive(l.name, -1)
	}
	if st.gSource {
		l.perSrc.Release(st.peer.Addr())
	}
	if st.gListener {
		l.gate.Release()
	}
	if st.gGlobal {
		l.global.Release()
	}
	l.finish(st)
}

// finish emits the per-connection metrics and the one access-log record.
//
// Metrics model: connections_accepted counts connections that passed the ACL
// and the limits. connections_rejected counts every connection closed before
// proxying, with its reason: acl and limit_* (never accepted) and the
// post-accept reasons draining, proxy_header, sni_*, no_backend, dial_failed,
// backend_write, panic. ConnClosed is emitted only for connections that
// reached pipe.Run. So accepted == closed + rejected(post-accept reasons).
func (l *Listener) finish(st *connState) {
	d := time.Since(st.start)
	if st.reason != "" {
		l.rec.ConnRejected(l.name, st.reason)
	}
	if st.proxied {
		l.rec.ConnClosed(l.name, st.result, d, st.in, st.out)
	}

	rec := accesslog.Record{
		Time:     st.start,
		Listener: l.name,
		Protocol: string(l.proto),
		Pool:     st.poolKey,
		Client:   st.peer.String(),
		ProxySrc: st.proxySrc,
		SNI:      st.sni,
		Duration: d,
		BytesIn:  st.in,
		BytesOut: st.out,
		Result:   st.result,
		Error:    st.errStr,
		Mode:     st.mode,
	}
	if st.b != nil {
		rec.Backend = st.b.Address()
	}
	if !st.peer.IsValid() && st.client != nil {
		rec.Client = st.client.RemoteAddr().String()
	}
	if st.client != nil {
		rec.Local = st.client.LocalAddr().String()
	}
	l.alog.Log(rec)
}
