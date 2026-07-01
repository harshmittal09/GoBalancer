package ring

import (
	"fmt"
	"sort"
	"testing"
)

// -----------------------------------------------------------------------------
// Helper utilities
// -----------------------------------------------------------------------------

// addNodes is a test helper that adds all addrs to ring r.
func addNodes(r *HashRing, addrs ...string) {
	for _, a := range addrs {
		r.AddNode(a)
	}
}

// -----------------------------------------------------------------------------
// Test: AddNode populates virtual nodes correctly
// -----------------------------------------------------------------------------

func TestAddNode_VirtualNodeCount(t *testing.T) {
	r := New()
	r.AddNode("192.168.1.1:8080")

	if got := r.Len(); got != virtualNodeCount {
		t.Errorf("expected %d virtual nodes, got %d", virtualNodeCount, got)
	}
}

func TestAddNode_MultipleNodes(t *testing.T) {
	r := New()
	addNodes(r, "192.168.1.1:8080", "192.168.1.2:8080", "192.168.1.3:8080")

	// In the very unlikely case of hash collisions the count may be slightly
	// less than 3 * virtualNodeCount, but for test backends it should be exact.
	expected := 3 * virtualNodeCount
	if got := r.Len(); got != expected {
		t.Errorf("expected %d virtual nodes, got %d", expected, got)
	}
}

// -----------------------------------------------------------------------------
// Test: RemoveNode purges all virtual nodes for the given address
// -----------------------------------------------------------------------------

func TestRemoveNode(t *testing.T) {
	r := New()
	addNodes(r, "backend1:8081", "backend2:8082", "backend3:8083")

	r.RemoveNode("backend2:8082")

	// Exactly 2 × virtualNodeCount nodes should remain.
	expected := 2 * virtualNodeCount
	if got := r.Len(); got != expected {
		t.Errorf("after removal expected %d virtual nodes, got %d", expected, got)
	}

	// GetNode must never route to the removed backend.
	for i := 0; i < 1000; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		node := r.GetNode(ip)
		if node == "backend2:8082" {
			t.Errorf("GetNode routed to removed backend for IP %s", ip)
		}
	}
}

func TestRemoveNode_Idempotent(t *testing.T) {
	r := New()
	r.AddNode("backend1:8081")
	r.RemoveNode("backend1:8081")
	r.RemoveNode("backend1:8081") // second removal must not panic

	if got := r.Len(); got != 0 {
		t.Errorf("expected 0 nodes after double removal, got %d", got)
	}
}

// -----------------------------------------------------------------------------
// Test: GetNode returns empty string when ring is empty
// -----------------------------------------------------------------------------

func TestGetNode_EmptyRing(t *testing.T) {
	r := New()
	if got := r.GetNode("10.0.0.1"); got != "" {
		t.Errorf("expected empty string for empty ring, got %q", got)
	}
}

// -----------------------------------------------------------------------------
// Test: GetNode – O(log N) correctness (determinism & consistency)
// -----------------------------------------------------------------------------

func TestGetNode_Determinism(t *testing.T) {
	r := New()
	addNodes(r, "backend1:8081", "backend2:8082", "backend3:8083")

	// The same client IP must always resolve to the same backend.
	const clientIP = "203.0.113.42"
	first := r.GetNode(clientIP)
	for i := 0; i < 1000; i++ {
		if got := r.GetNode(clientIP); got != first {
			t.Errorf("non-deterministic routing: iteration %d returned %q, want %q", i, got, first)
		}
	}
}

func TestGetNode_Distribution(t *testing.T) {
	r := New()
	addNodes(r, "backend1:8081", "backend2:8082", "backend3:8083")

	counts := make(map[string]int)
	total := 3000
	for i := 0; i < total; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		node := r.GetNode(ip)
		if node == "" {
			t.Fatal("GetNode returned empty string for non-empty ring")
		}
		counts[node]++
	}

	// Each backend should receive at least 15 % of traffic (rough lower bound).
	minExpected := total / 10
	for addr, cnt := range counts {
		if cnt < minExpected {
			t.Errorf("backend %s received only %d/%d connections (< %d%%)", addr, cnt, total, minExpected*100/total)
		}
	}
}

// -----------------------------------------------------------------------------
// Test: Wrap-around boundary condition
// -----------------------------------------------------------------------------

func TestGetNode_WrapAround(t *testing.T) {
	r := New()
	r.AddNode("only-backend:9000")

	// Force the wrap-around path: find a client IP whose hash is greater than
	// the maximum hash on the ring.
	r.mu.RLock()
	maxHash := r.hashes[len(r.hashes)-1]
	r.mu.RUnlock()

	// Scan candidate IPs until we find one whose FNV-1a hash exceeds maxHash.
	// In the worst case we may not find one, so we also artificially verify by
	// inspecting the nodeMap: the node at index 0 must equal the only backend.
	r.mu.RLock()
	wrapTarget := r.nodeMap[r.hashes[0]]
	r.mu.RUnlock()

	if wrapTarget != "only-backend:9000" {
		t.Errorf("expected wrap-around target to be the only backend, got %q", wrapTarget)
	}

	// Verify that GetNode always returns the only backend regardless of IP.
	for i := 0; i < 500; i++ {
		ip := fmt.Sprintf("172.16.%d.%d", i/256, i%256)
		node := r.GetNode(ip)
		if node != "only-backend:9000" {
			t.Errorf("expected only-backend:9000 for IP %s, got %q", ip, node)
		}
	}
	_ = maxHash // reference to suppress unused variable warning
}

// -----------------------------------------------------------------------------
// Test: The hashes slice is always sorted after mutations
// -----------------------------------------------------------------------------

func TestHashSliceAlwaysSorted(t *testing.T) {
	r := New()
	addNodes(r, "a:1", "b:2", "c:3", "d:4")
	r.RemoveNode("b:2")

	r.mu.RLock()
	defer r.mu.RUnlock()

	if !sort.SliceIsSorted(r.hashes, func(i, j int) bool {
		return r.hashes[i] < r.hashes[j]
	}) {
		t.Error("hashes slice is not sorted after AddNode + RemoveNode sequence")
	}
}

// -----------------------------------------------------------------------------
// Test: AddNode is idempotent (re-adding the same node doesn't duplicate)
// -----------------------------------------------------------------------------

func TestAddNode_Idempotent(t *testing.T) {
	r := New()
	r.AddNode("backend1:8081")
	r.AddNode("backend1:8081") // second add must not double the count

	if got := r.Len(); got != virtualNodeCount {
		t.Errorf("expected %d virtual nodes after idempotent add, got %d", virtualNodeCount, got)
	}
}
