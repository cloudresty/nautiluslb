package balancer

import (
	"hash/fnv"
	"slices"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

const (
	vnodesPerWeight = 160
	// maxHashWeight caps the weight used for ring size (risk R-F): weight 100 x
	// 50 backends would be 800k entries. Weights above 10 still count as 10.
	maxHashWeight = 10
)

// sourceIPHash is a ketama-style consistent hash ring over the source IP. The
// ring is built in Rebuild from ALL backends (stable Endpoint address and
// static weight; vnodes = 160 x min(weight,10)), so health transitions never
// change it: Pick walks the ring skipping unhealthy and over-cap backends,
// which yields the same owner as a ring built from the healthy set alone.
// Rebuild is a cheap no-op (the ring is reused, only the backend pointers are
// refreshed) while the (address, weight) membership is unchanged. Slow-start is
// deliberately ignored so a recovering backend does not reshuffle clients.
// FNV-1a 64 plus a fixed finalizer is process-independent, so every instance
// agrees on the mapping (hash/maphash is seeded per process and would not).
//
// Pick never rebuilds: a Pick holding an older view uses the latest ring
// (stale by microseconds at worst; the publisher rebuilds before storing the
// view). The one exception is a Pick before any Rebuild, which builds once.
type sourceIPHash struct {
	state atomic.Pointer[hashState]
}

type member struct {
	addr   string
	weight int
}

type hashState struct {
	members []member
	all     []*backend.Backend
	ring    []ringEntry
}

type ringEntry struct {
	hash uint64
	idx  int32
}

func mix(x uint64) uint64 { // murmur3 fmix64: FNV-1a alone clusters on near-identical inputs
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

func fnv64(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return mix(h.Sum64())
}

func (s *sourceIPHash) Rebuild(snap *backend.Snapshot) {
	if old := s.state.Load(); old != nil && sameMembers(old.members, snap.All) {
		if len(old.all) == len(snap.All) && (len(snap.All) == 0 || &old.all[0] == &snap.All[0]) {
			return
		}
		// Same membership, possibly new Backend objects: reuse the ring.
		s.state.Store(&hashState{members: old.members, all: snap.All, ring: old.ring})
		return
	}
	s.state.Store(buildRing(snap.All))
}

func sameMembers(m []member, all []*backend.Backend) bool {
	if len(m) != len(all) {
		return false
	}
	for i, b := range all {
		if m[i].addr != b.Address() || m[i].weight != hashWeight(b) {
			return false
		}
	}
	return true
}

func hashWeight(b *backend.Backend) int { return min(max(b.Endpoint().Weight, 1), maxHashWeight) }

func buildRing(all []*backend.Backend) *hashState {
	st := &hashState{all: all, members: make([]member, len(all))}
	for i, b := range all {
		w := hashWeight(b)
		addr := b.Address()
		st.members[i] = member{addr, w}
		for v := 0; v < vnodesPerWeight*w; v++ {
			st.ring = append(st.ring, ringEntry{fnv64([]byte(addr + "#" + strconv.Itoa(v))), int32(i)})
		}
	}
	slices.SortFunc(st.ring, func(a, b ringEntry) int {
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		}
		return int(a.idx - b.idx) // deterministic on collision
	})
	return st
}

func (s *sourceIPHash) Pick(snap *backend.Snapshot, key Key, n int) []*backend.Backend {
	if n <= 0 || len(snap.Healthy) == 0 {
		return nil
	}
	st := s.state.Load()
	if st == nil {
		s.state.CompareAndSwap(nil, buildRing(snap.All))
		st = s.state.Load()
	}
	// Fail-open view: the pool reuses the All slice as Healthy.
	failOpen := len(snap.Healthy) == len(snap.All) && &snap.Healthy[0] == &snap.All[0]
	var kb [16]byte
	ip := key.SourceIP.Unmap()
	k := fnv64(append(kb[:0], ip.AsSlice()...))
	pos := sort.Search(len(st.ring), func(i int) bool { return st.ring[i].hash >= k })
	n = min(n, len(st.all))
	out := make([]*backend.Backend, 0, n)
	for i := 0; i < len(st.ring) && len(out) < n; i++ {
		b := st.all[st.ring[(pos+i)%len(st.ring)].idx]
		if contains(out, b) || full(b) || (!failOpen && !b.IsHealthy()) {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
