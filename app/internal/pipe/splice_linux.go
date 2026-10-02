//go:build linux

package pipe

import (
	"io"
	"net"
)

// spliceTestHook, when set, is told what the splice path hands to ReadFrom.
// It exists so tests can assert the raw *net.TCPConn reaches ReadFrom with no
// wrapper in between (a wrapper would silently disable splice(2)).
var spliceTestHook func(dst *net.TCPConn, src io.Reader)

// newEntry selects the splice path: both conns must be raw *net.TCPConn, the
// watchdog must be running and the kernel must expose TCP_INFO byte counters.
func newEntry(client, upstream net.Conn, w *Watchdog) (*entry, bool) {

	if !w.active() {
		return nil, false
	}
	c, ok := client.(*net.TCPConn)
	if !ok {
		return nil, false
	}
	u, ok := upstream.(*net.TCPConn)
	if !ok {
		return nil, false
	}
	cs, err := readTCPInfo(c)
	if err != nil {
		return nil, false
	}
	us, err := readTCPInfo(u)
	if err != nil {
		return nil, false
	}
	return &entry{sample: [2]sampler{cs, us}}, true

}

// spliceCopy copies with dst.ReadFrom(src) on the RAW *net.TCPConn values so
// the runtime uses splice(2); no deadline is set, the Watchdog bounds the pipe
// and interrupts a blocked ReadFrom by closing both conns.
func spliceCopy(dst, src net.Conn, _ bool) (int64, error) {

	d, s := dst.(*net.TCPConn), src.(*net.TCPConn)
	if h := spliceTestHook; h != nil {
		h(d, s)
	}
	return d.ReadFrom(s)

}
