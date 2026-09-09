package hashring

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestEmptyRing(t *testing.T) {
	if _, err := New(16, 1.25).Pick("k"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Pick on empty ring = %v, want ErrEmpty", err)
	}
}

func TestStickyWhenThereIsSlack(t *testing.T) {
	r := New(128, 1.25)
	for i := range 4 {
		r.Add(fmt.Sprintf("node-%d", i))
	}
	// With load released after each request nothing is ever near capacity, so the
	// same key must keep landing on the same node.
	first, err := r.Pick("stable-key")
	if err != nil {
		t.Fatal(err)
	}
	r.Done(first)

	for range 100 {
		got, err := r.Pick("stable-key")
		if err != nil {
			t.Fatal(err)
		}
		r.Done(got)
		if got != first {
			t.Fatalf("key moved from %s to %s with an idle ring", first, got)
		}
	}
}

func TestKeysSpreadAcrossNodes(t *testing.T) {
	const nodes = 8
	r := New(256, 1.25)
	for i := range nodes {
		r.Add(fmt.Sprintf("node-%d", i))
	}

	counts := map[string]int{}
	const requests = 80_000
	for i := range requests {
		n, err := r.Pick(fmt.Sprintf("key-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		counts[n]++
		r.Done(n)
	}

	ideal := float64(requests) / nodes
	for name, got := range counts {
		if dev := math.Abs(float64(got)-ideal) / ideal; dev > 0.15 {
			t.Errorf("%s got %d of %d keys, %.1f%% off ideal", name, got, requests, dev*100)
		}
	}
}

// TestHotKeyIsBounded is the property the whole package exists for. All load
// targets one key, so plain consistent hashing would pin every request to one
// node. The bound must spread it.
func TestHotKeyIsBounded(t *testing.T) {
	const nodes = 4
	r := New(256, 1.25)
	for i := range nodes {
		r.Add(fmt.Sprintf("node-%d", i))
	}

	// Hold every request open so load accumulates.
	const inflight = 400
	for range inflight {
		if _, err := r.Pick("one-hot-key"); err != nil {
			t.Fatal(err)
		}
	}

	loads := r.Loads()
	var maxLoad int64
	for _, l := range loads {
		if l > maxLoad {
			maxLoad = l
		}
	}

	avg := float64(inflight) / nodes
	// factor 1.25 plus the formula's +1 slack, with room for the concurrent
	// fallback path. A plain ring would show maxLoad == inflight.
	if limit := int64(avg*1.25) + 2; maxLoad > limit {
		t.Errorf("hottest node carries %d of %d requests, bound is %d: loads=%v", maxLoad, inflight, limit, loads)
	}
	if maxLoad == inflight {
		t.Error("all load landed on one node: the bound is not being applied at all")
	}
}

func TestAddingANodeMovesAboutOneOverN(t *testing.T) {
	r := New(256, 1.25)
	for i := range 4 {
		r.Add(fmt.Sprintf("node-%d", i))
	}

	const keys = 20_000
	before := make([]string, keys)
	for i := range keys {
		n, _ := r.Pick(fmt.Sprintf("key-%d", i))
		r.Done(n)
		before[i] = n
	}

	r.Add("node-4")

	moved := 0
	for i := range keys {
		n, _ := r.Pick(fmt.Sprintf("key-%d", i))
		r.Done(n)
		if n != before[i] {
			moved++
		}
	}

	frac := float64(moved) / keys
	// Ideal is 1/5 = 0.20. Anything near 0.8 would mean the ring reshuffled, which
	// in production is a cluster-wide cache flush.
	if frac < 0.10 || frac > 0.30 {
		t.Errorf("adding a 5th node moved %.1f%% of keys, want roughly 20%%", frac*100)
	}
}

func TestConcurrentPickIsRaceFree(t *testing.T) {
	r := New(128, 1.25)
	for i := range 4 {
		r.Add(fmt.Sprintf("node-%d", i))
	}

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 2_000 {
				n, err := r.Pick(fmt.Sprintf("key-%d-%d", w, i))
				if err != nil {
					t.Error(err)
					return
				}
				r.Done(n)
			}
		}(w)
	}
	wg.Wait()

	// Every Pick was matched by a Done, so the ring must be idle again. A leak here
	// would slowly make every node look full and degrade routing into round robin.
	for name, l := range r.Loads() {
		if l != 0 {
			t.Errorf("%s has residual load %d after all requests completed", name, l)
		}
	}
}

func BenchmarkPick(b *testing.B) {
	r := New(256, 1.25)
	for i := range 8 {
		r.Add(fmt.Sprintf("node-%d", i))
	}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			n, err := r.Pick(keys[i&1023])
			if err != nil {
				b.Fatal(err)
			}
			r.Done(n)
			i++
		}
	})
}
