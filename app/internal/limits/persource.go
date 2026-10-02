package limits

import (
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
)

const shardCount = 64

type shard struct {
	mu sync.Mutex
	m  map[netip.Addr]int32
}

// PerSource bounds concurrent holders per client IP. IPv4-mapped IPv6
// addresses are normalised so a client counts once. Capacity 0 = unlimited
// and keeps no state.
type PerSource struct {
	capacity atomic.Int32
	shards   [shardCount]shard
}

// NewPerSource returns a PerSource with the given per-IP capacity (0 = unlimited).
func NewPerSource(capacity int) *PerSource {
	p := &PerSource{}
	p.capacity.Store(clampInt32(capacity))
	for i := range p.shards {
		p.shards[i].m = make(map[netip.Addr]int32)
	}
	return p
}

// SetCapacity changes the per-IP capacity (0 = unlimited). Existing holders
// are unaffected.
func (p *PerSource) SetCapacity(n int) { p.capacity.Store(clampInt32(n)) }

// clampInt32 narrows n to int32, saturating instead of wrapping.
func clampInt32(n int) int32 {
	return int32(min(max(n, math.MinInt32), math.MaxInt32)) //nolint:gosec // G115: clamped to the int32 range on the line above
}

func norm(ip netip.Addr) netip.Addr { return ip.Unmap().WithZone("") }

func (p *PerSource) shardFor(ip netip.Addr) *shard {
	b := ip.As16()
	h := uint32(2166136261) // FNV-1a
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return &p.shards[h%shardCount]
}

// TryAcquire takes a slot for ip if it is below capacity.
func (p *PerSource) TryAcquire(ip netip.Addr) bool {
	c := p.capacity.Load()
	if c <= 0 {
		return true
	}
	ip = norm(ip)
	s := p.shardFor(ip)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[ip] >= c {
		return false
	}
	s.m[ip]++
	return true
}

// Release returns a slot for ip, deleting the entry at zero. Releasing an
// unknown ip is a no-op.
func (p *PerSource) Release(ip netip.Addr) {
	ip = norm(ip)
	s := p.shardFor(ip)
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[ip]
	if !ok {
		return
	}
	if n <= 1 {
		delete(s.m, ip)
		return
	}
	s.m[ip] = n - 1
}

// len returns the total number of tracked IPs (tests only).
func (p *PerSource) len() int {
	total := 0
	for i := range p.shards {
		s := &p.shards[i]
		s.mu.Lock()
		total += len(s.m)
		s.mu.Unlock()
	}
	return total
}
