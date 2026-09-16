package cache

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeClock advances one millisecond per read, which is enough for LRU to have a
// strict ordering without any test sleeping.
type fakeClock struct{ us atomic.Int64 }

func (c *fakeClock) nowUS() int64 { return c.us.Add(1000) }

func newTestCache(t *testing.T, capacity int64, shards int, policy func(int64) Policy) *Cache {
	t.Helper()
	clk := &fakeClock{}
	c, err := New(Config{
		NewPolicy:     policy,
		NowUS:         clk.nowUS,
		Nanos:         func() int64 { return 0 },
		CapacityBytes: capacity,
		Shards:        shards,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGetPutDelete(t *testing.T) {
	c := newTestCache(t, 1<<20, 1, func(int64) Policy { return NewLRU(5) })

	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get on empty cache reported a hit")
	}
	if !c.Put("a", []byte("hello"), 0) {
		t.Fatal("Put was not admitted")
	}
	got, ok := c.Get("a")
	if !ok || string(got) != "hello" {
		t.Fatalf("Get = %q, %v; want \"hello\", true", got, ok)
	}
	if !c.Delete("a") {
		t.Fatal("Delete reported the key was absent")
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get after Delete reported a hit")
	}
	if c.Delete("a") {
		t.Fatal("second Delete reported the key was present")
	}
}

func TestOverwriteDoesNotLeakCapacity(t *testing.T) {
	c := newTestCache(t, 1<<16, 1, func(int64) Policy { return NewLRU(5) })
	for range 100 {
		c.Put("k", make([]byte, 1000), 0)
	}
	st := c.Snapshot()
	if st.Objects != 1 {
		t.Fatalf("Objects = %d, want 1", st.Objects)
	}
	if st.BytesUsed != 1000 {
		t.Fatalf("BytesUsed = %d, want 1000: overwrite double-counted", st.BytesUsed)
	}
}

func TestCapacityIsEnforced(t *testing.T) {
	const capacity = 10_000
	const valueSize = 100
	c := newTestCache(t, capacity, 1, func(int64) Policy { return NewLRU(5) })

	for i := range 500 {
		c.Admit(fmt.Sprintf("key-%d", i), make([]byte, valueSize))
	}
	st := c.Snapshot()
	if st.BytesUsed > capacity {
		t.Fatalf("BytesUsed = %d exceeds capacity %d", st.BytesUsed, capacity)
	}
	if st.Evictions == 0 {
		t.Fatal("no evictions after inserting 50x the capacity")
	}
	if st.Objects != st.BytesUsed/valueSize {
		t.Fatalf("Objects = %d but BytesUsed = %d: accounting drifted", st.Objects, st.BytesUsed)
	}
}

func TestOversizedObjectIsRejectedNotThrashed(t *testing.T) {
	c := newTestCache(t, 1000, 1, func(int64) Policy { return NewLRU(5) })
	c.Put("small", make([]byte, 500), 0)

	if c.Put("huge", make([]byte, 2000), 0) {
		t.Fatal("an object larger than the shard was admitted")
	}
	if _, ok := c.Get("small"); !ok {
		t.Fatal("rejecting an oversized object evicted the existing entry")
	}
	if st := c.Snapshot(); st.Rejections != 1 {
		t.Fatalf("Rejections = %d, want 1", st.Rejections)
	}
}

func TestLRUEvictsTheColdestSampledEntry(t *testing.T) {
	// One shard, capacity for exactly three entries, sample the whole shard so the
	// choice is deterministic rather than approximate.
	c := newTestCache(t, 300, 1, func(int64) Policy { return NewLRU(64) })

	c.Put("a", make([]byte, 100), 0)
	c.Put("b", make([]byte, 100), 0)
	c.Put("c", make([]byte, 100), 0)

	// Touch a and b, leaving c the coldest.
	c.Get("a")
	c.Get("b")

	c.Put("d", make([]byte, 100), 0)

	if _, ok := c.Get("c"); ok {
		t.Error("c was the least recently used but survived")
	}
	for _, k := range []string{"a", "b", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s should still be cached", k)
		}
	}
}

func TestStatsRatios(t *testing.T) {
	c := newTestCache(t, 1<<20, 1, func(int64) Policy { return NewLRU(5) })
	c.Admit("a", make([]byte, 100))

	for range 3 {
		c.Get("a")
	}
	c.Get("absent")

	st := c.Snapshot()
	// 3 hits and 1 miss. Admit contributes miss *bytes* without a miss count,
	// because the Get that caused it already counted the miss.
	if st.Hits != 3 {
		t.Errorf("Hits = %d, want 3", st.Hits)
	}
	if got, want := st.ObjectHitRatio(), 3.0/4.0; got != want {
		t.Errorf("ObjectHitRatio = %v, want %v", got, want)
	}
	if got, want := st.ByteHitRatio(), 300.0/400.0; got != want {
		t.Errorf("ByteHitRatio = %v, want %v", got, want)
	}
}

func TestShardsAreRoundedToPowerOfTwo(t *testing.T) {
	c := newTestCache(t, 1<<20, 100, func(int64) Policy { return NewLRU(5) })
	if len(c.shards) != 128 {
		t.Fatalf("shards = %d, want 128", len(c.shards))
	}
}

func TestConcurrentAccessIsRaceFree(t *testing.T) {
	c := newTestCache(t, 1<<16, 64, func(int64) Policy { return NewLRU(5) })

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, 0x9e3779b9))
			for range 5_000 {
				k := fmt.Sprintf("key-%d", rng.IntN(500))
				if _, ok := c.Get(k); !ok {
					c.Admit(k, make([]byte, 64))
				}
			}
		}(uint64(w))
	}
	wg.Wait()

	if st := c.Snapshot(); st.BytesUsed > st.BytesCapacity {
		t.Fatalf("BytesUsed %d over capacity %d after concurrent load", st.BytesUsed, st.BytesCapacity)
	}
}

func TestHashIsStableAndSpreadsAcrossShards(t *testing.T) {
	// A golden value, not a self-comparison: the gateway hashes a key in one
	// process and a cache node re-derives the shard from it in another, so a change
	// to Hash silently reshuffles every key in a running cluster.
	if got, want := Hash("belady"), uint64(15632626156217396277); got != want {
		t.Fatalf("Hash(\"belady\") = %d, want %d: the key mapping changed", got, want)
	}
	// Low bits pick the shard, so keys differing only in their tail must not pile
	// into one shard.
	const shards = 64
	counts := make([]int, shards)
	for i := range 64_000 {
		counts[Hash(fmt.Sprintf("key-%d", i))&(shards-1)]++
	}
	for i, n := range counts {
		if n < 700 || n > 1300 {
			t.Fatalf("shard %d got %d of 64000 keys: low-bit avalanche is poor", i, n)
		}
	}
}
