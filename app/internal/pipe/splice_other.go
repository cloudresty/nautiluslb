//go:build !linux

package pipe

import "net"

// newEntry never selects the splice path off linux.
func newEntry(_, _ net.Conn, _ *Watchdog) (*entry, bool) { return nil, false }

// spliceCopy is unreachable off linux (newEntry never succeeds).
func spliceCopy(_, _ net.Conn, _ bool) (int64, error) {
	panic("pipe: splice path selected on a non-linux platform")
}
