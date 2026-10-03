// Command idleconn opens N idle TCP connections to one address, holds them
// until SIGTERM/SIGINT, then closes them all and exits. It never sends a
// byte. Standard library only, so it builds without a go.mod:
//
//	CGO_ENABLED=0 GOOS=linux go build -o idleconn test/perf/idleconn/main.go
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "", "host:port to connect to")
	n := flag.Int("n", 10000, "connections to open")
	workers := flag.Int("workers", 64, "concurrent dialers")
	flag.Parse()
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "-addr is required")
		os.Exit(2)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	var (
		mu     sync.Mutex
		conns  = make([]net.Conn, 0, *n)
		failed atomic.Int64
		next   atomic.Int64
		wg     sync.WaitGroup
	)
	start := time.Now()
	d := net.Dialer{Timeout: 10 * time.Second}
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int64(*n) {
				c, err := d.Dial("tcp", *addr)
				if err != nil {
					if failed.Add(1) <= 5 {
						fmt.Fprintln(os.Stderr, "dial:", err)
					}
					continue
				}
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Printf("established %d failed %d in %s\n", len(conns), failed.Load(), time.Since(start).Round(time.Millisecond))

	// Detect connections the far side closed while we hold them: a read
	// returning anything (EOF included) means the proxy or backend dropped it.
	var dropped atomic.Int64
	for _, c := range conns {
		go func(c net.Conn) {
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err != nil && !isClosed(err) {
				dropped.Add(1)
			}
		}(c)
	}

	<-sig
	fmt.Printf("dropped-while-held %d\n", dropped.Load())
	for _, c := range conns {
		_ = c.Close()
	}
	fmt.Printf("closed %d\n", len(conns))
}

func isClosed(err error) bool { return errors.Is(err, net.ErrClosed) }
