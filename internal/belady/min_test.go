package belady

import (
	"math/rand/v2"
	"testing"
)

// The reference string from Silberschatz's page-replacement chapter, where the
// optimal policy is known to take exactly 9 faults on 3 frames. 20 requests minus
// 9 faults is 11 hits.
var textbook = []uint64{7, 0, 1, 2, 0, 3, 0, 4, 2, 3, 0, 3, 2, 1, 2, 0, 1, 7, 0, 1}

func TestMINMatchesTheTextbookOptimum(t *testing.T) {
	got := MINHitRatio(textbook, 3)
	want := 11.0 / 20.0
	if got != want {
		t.Fatalf("MINHitRatio = %v (%.0f hits), want %v (11 hits)", got, got*20, want)
	}
}

func TestMINEdgeCases(t *testing.T) {
	if got := MINHitRatio(nil, 10); got != 0 {
		t.Errorf("empty trace = %v, want 0", got)
	}
	if got := MINHitRatio(textbook, 0); got != 0 {
		t.Errorf("zero capacity = %v, want 0", got)
	}
	// A cache larger than the working set never evicts, so every request after the
	// first for each key is a hit: 20 requests over 6 distinct keys.
	if got, want := MINHitRatio(textbook, 100), 14.0/20.0; got != want {
		t.Errorf("unbounded capacity = %v, want %v", got, want)
	}
	// Capacity of one: a hit only when a key repeats immediately, which never
	// happens in this trace.
	if got := MINHitRatio(textbook, 1); got != 0 {
		t.Errorf("capacity 1 = %v, want 0", got)
	}
}

func TestMINIsMonotonicInCapacity(t *testing.T) {
	trace := randomTrace(20_000, 2_000)
	prev := -1.0
	for _, cap := range []int{1, 2, 4, 8, 16, 64, 256, 1024, 4096} {
		got := MINHitRatio(trace, cap)
		if got < prev {
			t.Fatalf("hit ratio fell from %.4f to %.4f as capacity grew to %d", prev, got, cap)
		}
		prev = got
	}
}

// TestMINBeatsLRU is the assertion that makes MIN worth computing. If any online
// policy ever scored above it, the ceiling would be wrong and every comparison
// built on it would be meaningless.
func TestMINBeatsLRU(t *testing.T) {
	for _, keyspace := range []int{100, 1_000, 10_000} {
		trace := randomTrace(50_000, keyspace)
		for _, capacity := range []int{16, 128, 1024} {
			min := MINHitRatio(trace, capacity)
			lru := lruHitRatio(trace, capacity)
			if lru > min {
				t.Errorf("keyspace %d capacity %d: LRU %.4f beat MIN %.4f", keyspace, capacity, lru, min)
			}
		}
	}
}

// TestMINStaysAboveWarmLRU is the same guarantee under the warmup prefix that
// loadgen feeds it. Scoring a warm policy against a cold optimum is what made the
// first end-to-end run report a policy above the ceiling.
func TestMINStaysAboveWarmLRU(t *testing.T) {
	const warmup = 5_000
	trace := randomTrace(50_000, 20_000)
	scored := append(append([]uint64{}, trace[:warmup]...), trace...)

	for _, capacity := range []int{64, 512, 4096} {
		min := MINHitRatioFrom(scored, capacity, warmup)
		lru := lruHitRatioFrom(scored, capacity, warmup)
		if lru > min {
			t.Errorf("capacity %d: warm LRU %.4f beat warm MIN %.4f", capacity, lru, min)
		}
		// A warm cache must score at least as well as a cold one over the same
		// requests, otherwise the prefix is not reaching the simulation.
		if cold := MINHitRatio(trace, capacity); min < cold {
			t.Errorf("capacity %d: warm MIN %.4f below cold MIN %.4f", capacity, min, cold)
		}
	}
}

// lruHitRatio is an exact LRU, only for use as a lower bound in tests. The
// production policy in internal/cache samples candidates instead.
func lruHitRatio(trace []uint64, capacity int) float64 {
	return lruHitRatioFrom(trace, capacity, 0)
}

func lruHitRatioFrom(trace []uint64, capacity, from int) float64 {
	order := make([]uint64, 0, capacity)
	resident := make(map[uint64]struct{}, capacity)
	hits := 0

	for i, key := range trace {
		if _, ok := resident[key]; ok {
			if i >= from {
				hits++
			}
			for i, k := range order {
				if k == key {
					order = append(order[:i], order[i+1:]...)
					break
				}
			}
		} else {
			if len(order) >= capacity {
				delete(resident, order[0])
				order = order[1:]
			}
			resident[key] = struct{}{}
		}
		order = append(order, key)
	}
	return float64(hits) / float64(len(trace)-from)
}

func randomTrace(n, keyspace int) []uint64 {
	rng := rand.New(rand.NewPCG(7, 11))
	zipf := rand.NewZipf(rng, 1.2, 1, uint64(keyspace-1))
	trace := make([]uint64, n)
	for i := range trace {
		trace[i] = zipf.Uint64()
	}
	return trace
}

func BenchmarkMINHitRatio(b *testing.B) {
	trace := randomTrace(200_000, 100_000)
	b.ReportAllocs()
	for b.Loop() {
		MINHitRatio(trace, 10_000)
	}
}
