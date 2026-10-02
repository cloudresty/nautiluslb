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
// ring is built in Rebuild from the stable Endpoint address and static weight
// (vnodes = 160 x min(weight,10)); slow-start is deliberately ignored so a
// backend recovering does not reshuffle clients. FNV-1a 64 plus a fixed
// finalizer is process-independent, so every instance agrees on the mapping
// (hash/maphash is seeded per process and would not).
type sourceIPHash struct {
	state atomic.Pointer[hashState]
}

type hashState struct {
	id   ident
	h    []*backend.Backend
	ring []ringEntry
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
	s.state.Store(buildRing(snap.Healthy))
}

func buildRing(h []*backend.Backend) *hashState {
	st := &hashState{id: identOf(h), h: h}
	for i, b := range h {
		w := min(max(b.Endpoint().Weight, 1), maxHashWeight)
		addr := b.Address()
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
	if st == nil || st.id != identOf(snap.Healthy) {
		st = buildRing(snap.Healthy)
		s.state.Store(st)
	}
	var kb [16]byte
	ip := key.SourceIP.Unmap()
	k := fnv64(append(kb[:0], ip.AsSlice()...))
	pos := sort.Search(len(st.ring), func(i int) bool { return st.ring[i].hash >= k })
	n = min(n, len(st.h))
	out := make([]*backend.Backend, 0, n)
	for i := 0; i < len(st.ring) && len(out) < n; i++ {
		b := st.h[st.ring[(pos+i)%len(st.ring)].idx]
		if contains(out, b) || full(b) {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
