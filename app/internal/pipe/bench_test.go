package pipe

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
)

func benchRun(b *testing.B, mode string) {
	pool := &sync.Pool{New: func() any { buf := make([]byte, copyBufferSize); return &buf }}
	payload := bytes.Repeat([]byte("p"), 64<<10)
	b.SetBytes(int64(2 * len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		r := newRig(b)
		opts := Options{BufferPool: pool}
		if mode == ModeSplice {
			w := NewWatchdog(0, 0)
			w.Start(context.Background())
			defer w.Stop()
			opts.Watchdog = w
		}
		res := runAsync(r.client, r.upstream, opts)
		go echoBackend(r.backendPeer)
		go func() {
			_, _ = r.clientPeer.Write(payload)
			_ = r.clientPeer.CloseWrite()
		}()
		_, _ = io.Copy(io.Discard, r.clientPeer)
		<-res
		_ = r.clientPeer.Close()
		_ = r.client.Close()
		_ = r.upstream.Close()
		_ = r.backendPeer.Close()
	}
}

func BenchmarkRunGeneric(b *testing.B) { benchRun(b, ModeGeneric) }

// benchCopyThroughput streams 64MB client -> backend through Run over
// loopback TCP; the backend peer discards.
func benchCopyThroughput(b *testing.B, mode string) {
	const total = 64 << 20
	chunk := bytes.Repeat([]byte("x"), 256<<10)
	pool := &sync.Pool{New: func() any { buf := make([]byte, copyBufferSize); return &buf }}
	b.SetBytes(total)
	b.ReportAllocs()
	for b.Loop() {
		r := newRig(b)
		opts := Options{BufferPool: pool}
		if mode == ModeSplice {
			w := NewWatchdog(0, 0)
			w.Start(context.Background())
			opts.Watchdog = w
			defer w.Stop()
		}
		res := runAsync(r.client, r.upstream, opts)
		go func() {
			for sent := 0; sent < total; sent += len(chunk) {
				if _, err := r.clientPeer.Write(chunk); err != nil {
					break
				}
			}
			_ = r.clientPeer.CloseWrite()
		}()
		n, _ := io.Copy(io.Discard, r.backendPeer)
		if n != total {
			b.Fatalf("backend received %d, want %d", n, total)
		}
		_ = r.backendPeer.CloseWrite()
		_, _ = io.Copy(io.Discard, r.clientPeer)
		<-res
		for _, c := range []*net.TCPConn{r.clientPeer, r.client, r.upstream, r.backendPeer} {
			_ = c.Close()
		}
	}
}

func BenchmarkCopyThroughput(b *testing.B) { benchCopyThroughput(b, ModeGeneric) }
