package main

import (
	"context"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/cloudresty/emit"
)

// forceWait bounds how long supervise waits for shutdown to return after a
// second signal cancelled its context.
const forceWait = 5 * time.Second

// supervise runs the signal loop until a shutdown completes. SIGHUP and watch
// events trigger reload; reloads never run concurrently and are coalesced so
// at most one is pending behind the running one. SIGTERM/SIGINT runs shutdown
// with a context bounded by shutdownTimeout; a second SIGTERM/SIGINT cancels
// that context (force-close) and supervise returns once shutdown returns.
func supervise(sigs <-chan os.Signal, watch <-chan struct{}, reload func(), shutdown func(ctx context.Context), shutdownTimeout time.Duration) {
	pending := make(chan struct{}, 1)
	stopWorker := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopWorker:
				return
			case <-pending:
				reload()
			}
		}
	}()
	request := func() {
		select {
		case pending <- struct{}{}:
		default: // one reload already queued; it will read the latest file
		}
	}

	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				emit.Info.Msg("SIGHUP received, reloading configuration")
				request()
				continue
			}
			emit.Info.StructuredFields("Shutting down gracefully...", emit.ZString("signal", sig.String()))
			close(stopWorker)
			wg.Wait() // let an in-flight reload finish; shutdown must not race it
			runShutdown(sigs, shutdown, shutdownTimeout)
			return
		case <-watch:
			emit.Info.Msg("Configuration file changed, reloading")
			request()
		}
	}
}

func runShutdown(sigs <-chan os.Signal, shutdown func(ctx context.Context), timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdown(ctx)
	}()
	for {
		select {
		case <-done:
			return
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				continue
			}
			emit.Warn.StructuredFields("Second signal received, forcing shutdown", emit.ZString("signal", sig.String()))
			cancel()
			select {
			case <-done:
			case <-time.After(forceWait):
			}
			return
		}
	}
}

// watchFile polls path's mtime and size every interval and sends on out when
// either changes, until ctx is done.
func watchFile(ctx context.Context, path string, interval time.Duration, out chan<- struct{}) {
	type stamp struct {
		mod  time.Time
		size int64
	}
	read := func() (stamp, bool) {
		fi, err := os.Stat(path)
		if err != nil {
			return stamp{}, false
		}
		return stamp{fi.ModTime(), fi.Size()}, true
	}
	last, _ := read()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur, ok := read()
			if !ok || cur == last {
				continue
			}
			last = cur
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}
}
