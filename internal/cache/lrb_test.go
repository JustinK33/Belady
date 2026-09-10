package cache

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JustinK33/newproj/internal/features"
	"github.com/JustinK33/newproj/internal/model"
)

// staircase builds a model.txt whose prediction is a monotone decreasing step
// function of one feature: a right-leaning chain of splits at step intervals, with
// each successive leaf one lower than the last.
//
// Pointed at the accesses column it scores the least-accessed object highest, so the
// policy behaves exactly like LFU. That is the point: LFU and LRU disagree about
// which entry to evict on the trace below, so the test can tell whether the model was
// consulted at all rather than just watching the cache work.
func staircase(column, steps int, step float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tree\nnum_class=1\nnum_tree_per_iteration=1\nmax_feature_idx=%d\nobjective=binary sigmoid:1\n\n",
		features.Count-1)
	fmt.Fprintf(&b, "Tree=0\nnum_leaves=%d\nnum_cat=0\n", steps+1)

	split := make([]string, steps)
	thr := make([]string, steps)
	dt := make([]string, steps)
	left := make([]string, steps)
	right := make([]string, steps)
	leaves := make([]string, steps+1)
	for i := range steps {
		split[i] = fmt.Sprint(column)
		thr[i] = fmt.Sprintf("%.1f", float64(i)*step+step/2)
		dt[i] = "2"
		left[i] = fmt.Sprint(^int32(i)) // leaf i
		if i == steps-1 {
			right[i] = fmt.Sprint(^int32(steps)) // final leaf
		} else {
			right[i] = fmt.Sprint(i + 1)
		}
		leaves[i] = fmt.Sprint(-i)
	}
	leaves[steps] = fmt.Sprint(-steps)

	fmt.Fprintf(&b, "split_feature=%s\nthreshold=%s\ndecision_type=%s\nleft_child=%s\nright_child=%s\nleaf_value=%s\nis_linear=0\nshrinkage=1\n\nend of trees\n",
		strings.Join(split, " "), strings.Join(thr, " "), strings.Join(dt, " "),
		strings.Join(left, " "), strings.Join(right, " "), strings.Join(leaves, " "))
	return b.String()
}

// recencyHolder carries a model whose score rises with recency in half-second steps,
// which makes lrb behave roughly like LRU.
func recencyHolder() *ModelHolder {
	m, err := model.Load(strings.NewReader(staircase(features.RecencyMS, 24, 500)))
	if err != nil {
		panic(err)
	}
	h := NewModelHolder()
	if err := h.Store(m, "test-recency", time.Minute); err != nil {
		panic(err)
	}
	return h
}

// warmDisagreement leaves the cache holding four 100-byte objects where LRU and LFU
// pick different victims: w is the least recently used but the most frequently
// accessed, z is the opposite.
func warmDisagreement(t *testing.T, c *Cache) {
	t.Helper()
	val := make([]byte, 100)
	for _, k := range []string{"w", "x", "y", "z"} {
		if !c.Put(k, val, 0) {
			t.Fatalf("Put %s was rejected", k)
		}
	}
	for _, step := range []struct {
		key string
		n   int
	}{{"w", 5}, {"x", 4}, {"y", 3}, {"z", 2}} {
		for range step.n {
			if _, ok := c.Get(step.key); !ok {
				t.Fatalf("Get %s missed during warmup", step.key)
			}
		}
	}
}

func TestLRBFallsBackToLRUWithNoModel(t *testing.T) {
	holder := NewModelHolder()
	c := newTestCache(t, 400, 1, func(int64) Policy { return NewLRB(holder, 8) })
	if holder.Version() != "" {
		t.Fatalf("a fresh holder reports version %q", holder.Version())
	}

	warmDisagreement(t, c)
	if !c.Put("new", make([]byte, 100), 0) {
		t.Fatal("Put was rejected, so nothing was evicted")
	}

	// LRU semantics: w was touched first and never again.
	if _, ok := c.Get("w"); ok {
		t.Error("w survived, so the fallback did not evict by recency")
	}
	if _, ok := c.Get("z"); !ok {
		t.Error("z was evicted, which is the frequency-based choice, not the LRU one")
	}
}

func TestLRBUsesTheInstalledModel(t *testing.T) {
	m, err := model.Load(strings.NewReader(staircase(features.Accesses, 12, 1)))
	if err != nil {
		t.Fatal(err)
	}
	holder := NewModelHolder()
	if err := holder.Store(m, "test-lfu", time.Minute); err != nil {
		t.Fatal(err)
	}
	if holder.Version() != "test-lfu" || holder.Boundary() != time.Minute {
		t.Fatalf("holder reports %q / %v", holder.Version(), holder.Boundary())
	}

	c := newTestCache(t, 400, 1, func(int64) Policy { return NewLRB(holder, 8) })
	warmDisagreement(t, c)
	if !c.Put("new", make([]byte, 100), 0) {
		t.Fatal("Put was rejected, so nothing was evicted")
	}

	// The staircase scores fewest-accesses highest, so z must go and w must stay.
	if _, ok := c.Get("z"); ok {
		t.Error("z survived: the model was not consulted, or its score was inverted")
	}
	if _, ok := c.Get("w"); !ok {
		t.Error("w was evicted, which is the recency choice, so the fallback ran instead")
	}
}

func TestModelHolderRejectsMismatchedModels(t *testing.T) {
	holder := NewModelHolder()

	// A model trained on a different number of columns reads the wrong feature for
	// every split and would degrade the hit ratio with no other symptom.
	wrong := strings.Replace(staircase(0, 4, 1),
		fmt.Sprintf("max_feature_idx=%d", features.Count-1), "max_feature_idx=2", 1)
	m, err := model.Load(strings.NewReader(wrong))
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Store(m, "wrong-width", time.Minute); err == nil {
		t.Error("a model with 3 features was accepted")
	}

	ok, err := model.Load(strings.NewReader(staircase(features.Accesses, 4, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Store(ok, "no-boundary", 0); err == nil {
		t.Error("a model with a zero Belady boundary was accepted")
	}
	if holder.Version() != "" {
		t.Errorf("a rejected model was installed anyway: version %q", holder.Version())
	}
}

// TestLRBEvictionDoesNotAllocate guards the reason features.Input and the scratch
// vector are policy fields. An allocation here happens under the shard lock, on the
// path that runs when the cache is full, which is exactly when GC pressure is least
// affordable.
func TestLRBEvictionDoesNotAllocate(t *testing.T) {
	m, err := model.Load(strings.NewReader(staircase(features.Accesses, 12, 1)))
	if err != nil {
		t.Fatal(err)
	}
	holder := NewModelHolder()
	if err := holder.Store(m, "test", time.Minute); err != nil {
		t.Fatal(err)
	}

	s := newShard(4096, 8, NewLRB(holder, 8), func() int64 { return 0 }, nil)
	for i := range 40 {
		s.insert(uint64(i), make([]byte, 100), int64(i)*1000, 0, false)
	}

	n := testing.AllocsPerRun(200, func() {
		if _, ok := s.policy.Victim(s, 40_000); !ok {
			t.Fatal("no victim found")
		}
	})
	if n != 0 {
		t.Errorf("Victim allocated %v times per eviction", n)
	}
}

func BenchmarkLRBVictim(b *testing.B) {
	m, err := model.Load(strings.NewReader(staircase(features.Accesses, 12, 1)))
	if err != nil {
		b.Fatal(err)
	}
	holder := NewModelHolder()
	if err := holder.Store(m, "bench", time.Minute); err != nil {
		b.Fatal(err)
	}

	s := newShard(1<<20, 8, NewLRB(holder, 8), func() int64 { return 0 }, nil)
	for i := range 4096 {
		s.insert(uint64(i), make([]byte, 200), int64(i)*1000, 0, false)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, ok := s.policy.Victim(s, 1<<30); !ok {
			b.Fatal("no victim")
		}
	}
}
