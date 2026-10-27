package ring

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func testBackends(n int) []string {
	b := make([]string, n)
	for i := range b {
		b[i] = fmt.Sprintf("10.0.%d.%d:8080", i/250, i%250+1)
	}
	return b
}

// testKeys returns n random IPv4 addresses, seeded so runs are repeatable.
func testKeys(n int) []string {
	rng := rand.New(rand.NewSource(1))
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%d.%d.%d.%d", rng.Intn(223)+1, rng.Intn(256), rng.Intn(256), rng.Intn(256))
	}
	return keys
}

func newRing(backends []string) *Ring {
	r := New()
	for _, b := range backends {
		r.AddNode(b)
	}
	return r
}

func assign(r *Ring, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = r.GetNode(k)
	}
	return out
}

func TestEmptyRing(t *testing.T) {
	if got := New().GetNode("1.2.3.4"); got != "" {
		t.Fatalf("empty ring returned %q, want empty string", got)
	}
}

func TestAddIsIdempotent(t *testing.T) {
	r := newRing(testBackends(3))
	before := r.Len()
	r.AddNode(testBackends(3)[0])
	if r.Len() != before {
		t.Fatalf("re-adding a backend changed ring size from %d to %d", before, r.Len())
	}
}

func TestRemoveOnlyMovesKeysOfRemovedBackend(t *testing.T) {
	keys := testKeys(50000)
	for _, n := range []int{3, 5, 10} {
		bs := testBackends(n)
		r := newRing(bs)
		before := assign(r, keys)
		r.RemoveNode(bs[0])
		for i, k := range keys {
			if after := r.GetNode(k); after != before[i] && before[i] != bs[0] {
				t.Fatalf("%d backends: key %s moved from %s to %s, but %s was not removed", n, k, before[i], after, before[i])
			}
		}
	}
}

func TestAddOnlyMovesKeysToNewBackend(t *testing.T) {
	keys := testKeys(50000)
	for _, n := range []int{3, 5, 10} {
		bs := testBackends(n + 1)
		r := newRing(bs[:n])
		before := assign(r, keys)
		r.AddNode(bs[n])
		for i, k := range keys {
			if after := r.GetNode(k); after != before[i] && after != bs[n] {
				t.Fatalf("%d backends: key %s moved from %s to %s instead of the new backend", n, k, before[i], after)
			}
		}
	}
}

func TestReAddRestoresAssignments(t *testing.T) {
	keys := testKeys(50000)
	bs := testBackends(3)
	r := newRing(bs)
	before := assign(r, keys)
	r.RemoveNode(bs[1])
	r.AddNode(bs[1])
	for i, k := range keys {
		if after := r.GetNode(k); after != before[i] {
			t.Fatalf("key %s was on %s before remove/re-add, now on %s", k, before[i], after)
		}
	}
}

// TestDistributionReport logs how evenly keys spread across backends.
// It does not assert a bound yet; see "Known issues" in DESIGN.md.
// Run with -v to see the numbers.
func TestDistributionReport(t *testing.T) {
	keys := testKeys(200000)
	for _, n := range []int{3, 5, 10} {
		r := newRing(testBackends(n))
		count := map[string]int{}
		for _, k := range keys {
			count[r.GetNode(k)]++
		}
		mean := float64(len(keys)) / float64(n)
		var sumSq, maxLoad, minLoad float64 = 0, 0, math.MaxFloat64
		for _, c := range count {
			v := float64(c)
			sumSq += (v - mean) * (v - mean)
			maxLoad = math.Max(maxLoad, v)
			minLoad = math.Min(minLoad, v)
		}
		t.Logf("%2d backends: busiest/quietest = %.2fx, coefficient of variation = %.1f%%",
			n, maxLoad/minLoad, 100*math.Sqrt(sumSq/float64(n))/mean)
	}
}

func BenchmarkGetNode(b *testing.B) {
	keys := testKeys(100000)
	for _, n := range []int{3, 10, 50} {
		r := newRing(testBackends(n))
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r.GetNode(keys[i%len(keys)])
			}
		})
	}
}

func BenchmarkAddRemoveNode(b *testing.B) {
	for _, n := range []int{3, 10, 50} {
		bs := testBackends(n + 1)
		r := newRing(bs[:n])
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				r.AddNode(bs[n])
				r.RemoveNode(bs[n])
			}
		})
	}
}
