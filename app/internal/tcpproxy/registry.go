package tcpproxy

import (
	"net"
	"sync"
	"sync/atomic"
)

const registryShards = 16

// trackedConn is one registered client connection. Drain force-closes it
// through the registry; upstream is set once the backend dial succeeded.
type trackedConn struct {
	id       uint64
	client   net.Conn
	upstream atomic.Pointer[net.Conn]
}

func (t *trackedConn) closeAll() {
	_ = t.client.Close()
	if up := t.upstream.Load(); up != nil {
		_ = (*up).Close()
	}
}

type registryShard struct {
	mu sync.Mutex
	m  map[*trackedConn]struct{}
	_  [32]byte // keep shards off one cache line
}

// registry is the 16-shard connection set. It is the only place on the hot
// path that takes a mutex, and only for one map insert and one delete.
type registry struct {
	seq    atomic.Uint64
	active atomic.Int64
	shards [registryShards]registryShard
}

func newRegistry() *registry {
	r := &registry{}
	for i := range r.shards {
		r.shards[i].m = make(map[*trackedConn]struct{})
	}
	return r
}

func (r *registry) add(c net.Conn) *trackedConn {
	t := &trackedConn{id: r.seq.Add(1), client: c}
	s := &r.shards[t.id%registryShards]
	s.mu.Lock()
	s.m[t] = struct{}{}
	s.mu.Unlock()
	r.active.Add(1)
	return t
}

func (r *registry) remove(t *trackedConn) {
	s := &r.shards[t.id%registryShards]
	s.mu.Lock()
	delete(s.m, t)
	s.mu.Unlock()
	r.active.Add(-1)
}

// closeAll force-closes every registered connection and returns how many.
func (r *registry) closeAll() int {
	n := 0
	for i := range r.shards {
		s := &r.shards[i]
		s.mu.Lock()
		for t := range s.m {
			t.closeAll()
			n++
		}
		s.mu.Unlock()
	}
	return n
}
