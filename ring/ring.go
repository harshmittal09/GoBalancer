// Package ring implements a consistent hashing ring for distributing client
// connections across backend servers. It uses FNV-1a 32-bit hashing with 100
// virtual nodes per backend to achieve an even load distribution even with a
// small number of physical backends.
package ring

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
)

const virtualNodeCount = 100

// HashRing is a thread-safe consistent hashing ring.
// It maintains a sorted slice of virtual node hashes and a map from
// each hash to the physical backend address it represents.
type HashRing struct {
	mu      sync.RWMutex
	hashes  []uint32          // sorted slice of virtual node hash values
	nodeMap map[uint32]string // maps virtual node hash → physical backend address
}

// New creates and returns an initialised, empty HashRing.
func New() *HashRing {
	return &HashRing{
		hashes:  make([]uint32, 0),
		nodeMap: make(map[uint32]string),
	}
}

// hash computes the FNV-1a 32-bit hash of the given key string.
func hash(key string) uint32 {
	h := fnv.New32a()
	// fnv.New32a().Write never returns an error, so we discard it safely.
	_, _ = h.Write([]byte(key))
	return h.Sum32()
}

// AddNode registers a physical backend address on the ring by inserting
// virtualNodeCount virtual nodes, each identified by a hash of "addr#index".
// Duplicate calls with the same address are idempotent (virtual nodes that
// already exist in nodeMap will simply overwrite themselves).
func (r *HashRing) AddNode(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := 0; i < virtualNodeCount; i++ {
		key := fmt.Sprintf("%s#%d", addr, i)
		h := hash(key)

		// Only add the hash to the sorted slice if it is not already present.
		// This guards against accidental duplicate registration.
		if _, exists := r.nodeMap[h]; !exists {
			r.hashes = insertSorted(r.hashes, h)
		}
		r.nodeMap[h] = addr
	}
}

// RemoveNode deregisters all virtual nodes belonging to addr from the ring.
// After removal the ring is re-sorted (the slice rebuild guarantees correctness).
func (r *HashRing) RemoveNode(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Identify all hashes that belong to this physical address.
	toDelete := make(map[uint32]struct{})
	for h, a := range r.nodeMap {
		if a == addr {
			toDelete[h] = struct{}{}
		}
	}

	// Remove them from the map.
	for h := range toDelete {
		delete(r.nodeMap, h)
	}

	// Rebuild the sorted hashes slice without the removed entries.
	filtered := r.hashes[:0] // reuse underlying array
	for _, h := range r.hashes {
		if _, removed := toDelete[h]; !removed {
			filtered = append(filtered, h)
		}
	}
	r.hashes = filtered
}

// GetNode returns the physical backend address that should handle traffic from
// clientIP. The routing is deterministic: we hash the clientIP and walk the
// ring clockwise to find the nearest virtual node. If the client hash is larger
// than every node on the ring we wrap around to index 0 (the smallest hash).
//
// GetNode returns an empty string when the ring is empty (all backends are down).
func (r *HashRing) GetNode(clientIP string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.hashes) == 0 {
		return ""
	}

	clientHash := hash(clientIP)

	// sort.Search returns the smallest index i in [0, n) for which f(i) is true.
	// We look for the first virtual-node hash that is >= clientHash.
	idx := sort.Search(len(r.hashes), func(i int) bool {
		return r.hashes[i] >= clientHash
	})

	// Wrap-around: if clientHash is beyond the largest hash, route to index 0.
	if idx == len(r.hashes) {
		idx = 0
	}

	return r.nodeMap[r.hashes[idx]]
}

// Len returns the number of virtual nodes currently on the ring.
// Primarily useful for testing and diagnostics.
func (r *HashRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.hashes)
}

// Nodes returns the deduplicated list of physical backend addresses that are
// currently registered on the ring. The order is not guaranteed.
func (r *HashRing) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, addr := range r.nodeMap {
		seen[addr] = struct{}{}
	}

	result := make([]string, 0, len(seen))
	for addr := range seen {
		result = append(result, addr)
	}
	return result
}

// insertSorted inserts v into a sorted []uint32 and returns the new slice,
// maintaining ascending order. It uses binary search for O(log N) positioning.
func insertSorted(s []uint32, v uint32) []uint32 {
	idx := sort.Search(len(s), func(i int) bool { return s[i] >= v })
	// Grow the slice by one element.
	s = append(s, 0)
	// Shift elements to the right to make room.
	copy(s[idx+1:], s[idx:])
	s[idx] = v
	return s
}
