package cache

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// policies is the comparison set. Every entry must satisfy the same invariants,
// because the project's claim is that these are interchangeable under identical
// conditions and differ only in hit ratio and eviction cost.
var policies = map[string]func(int64) Policy{
	"lru":    func(int64) Policy { return NewLRU(8) },
	"lfu":    func(int64) Policy { return NewLFU(8) },
	"s3fifo": NewS3FIFO,
}

func TestPolicyConformance(t *testing.T) {
	for name, mk := range policies {
		t.Run(name, func(t *testing.T) {
			const capacity = 64 << 10
			c := newTestCache(t, capacity, 4, mk)

			if got := c.Policy(); got != name {
				t.Fatalf("Policy() = %q, want %q", got, name)
			}

			// Insert far more than fits, at mixed sizes, re-reading as we go.
			rng := rand.New(rand.NewPCG(1, 2))
			for i := range 20_000 {
				key := fmt.Sprintf("key-%d", rng.IntN(4_000))
				if _, ok := c.Get(key); !ok {
					c.Admit(key, make([]byte, 64+rng.IntN(512)))
				}
				if i%1000 == 0 {
					c.Delete(fmt.Sprintf("key-%d", rng.IntN(4_000)))
				}
			}

			st := c.Snapshot()
			if st.BytesUsed > st.BytesCapacity {
				t.Errorf("BytesUsed = %d over capacity %d", st.BytesUsed, st.BytesCapacity)
			}
			if st.Evictions == 0 {
				t.Error("no evictions despite heavy overcommit")
			}
			// A policy that stops naming victims silently turns the cache into a
			// read-through proxy, which is the failure that would otherwise look like
			// "the model is just not very good".
			if st.Rejections > st.Admissions/100 {
				t.Errorf("Rejections = %d against %d admissions: the policy is failing to find victims",
					st.Rejections, st.Admissions)
			}
			assertAccounting(t, c)
		})
	}
}

// assertAccounting verifies that the bytes each shard thinks it holds match the
// entries it actually holds. Drift here is how a cache slowly refuses to admit
// anything.
func assertAccounting(t *testing.T, c *Cache) {
	t.Helper()
	for i, s := range c.shards {
		s.mu.Lock()
		var sum int64
		for _, e := range s.m {
			sum += int64(e.size)
		}
		used := s.used
		s.mu.Unlock()

		if sum != used {
			t.Errorf("shard %d: entries total %d bytes but used = %d", i, sum, used)
		}
	}
}

func TestPolicyHitRatioOnZipf(t *testing.T) {
	for name, mk := range policies {
		t.Run(name, func(t *testing.T) {
			ratio := replayZipf(t, mk, 1<<20, 200_000, 100_000)
			t.Logf("%s object hit ratio %.4f", name, ratio)
			// A loose floor. The point is to catch a policy that has degenerated into
			// evicting whatever it just admitted, not to rank the policies here.
			if ratio < 0.20 {
				t.Errorf("object hit ratio %.4f is too low to be a working cache", ratio)
			}
		})
	}
}

// replayZipf drives a skewed workload of fixed-size objects and returns the
// object hit ratio. Fixed sizes keep object and byte hit ratio identical, so the
// number means one thing.
func replayZipf(t *testing.T, mk func(int64) Policy, capacity int64, requests, keyspace int) float64 {
	t.Helper()
	c := newTestCache(t, capacity, 8, mk)

	rng := rand.New(rand.NewPCG(42, 42))
	// s=1.1 is a typical web popularity skew: heavy head, very long tail.
	zipf := rand.NewZipf(rng, 1.1, 1, uint64(keyspace-1))
	value := make([]byte, 512)

	for range requests {
		key := fmt.Sprintf("key-%d", zipf.Uint64())
		if _, ok := c.Get(key); !ok {
			c.Admit(key, value)
		}
	}
	return c.Snapshot().ObjectHitRatio()
}

// TestScanResistance is the behavioural difference that matters. A burst of
// one-hit wonders should not flush an established working set. LRU has no defence
// against this; S3-FIFO's probationary queue is built for it.
func TestScanResistance(t *testing.T) {
	const (
		capacity  = 1 << 20
		valueSize = 512
		hotKeys   = 800 // ~40% of capacity
		scanKeys  = 4000
	)

	survival := map[string]float64{}
	for name, mk := range policies {
		c := newTestCache(t, capacity, 1, mk)
		value := make([]byte, valueSize)

		// Establish the working set, accessed enough times to look genuinely hot.
		for range 5 {
			for i := range hotKeys {
				key := fmt.Sprintf("hot-%d", i)
				if _, ok := c.Get(key); !ok {
					c.Admit(key, value)
				}
			}
		}

		// Scan: every key requested exactly once, never again.
		for i := range scanKeys {
			key := fmt.Sprintf("scan-%d", i)
			if _, ok := c.Get(key); !ok {
				c.Admit(key, value)
			}
		}

		var alive int
		for i := range hotKeys {
			if _, ok := c.Get(fmt.Sprintf("hot-%d", i)); ok {
				alive++
			}
		}
		survival[name] = float64(alive) / float64(hotKeys)
		t.Logf("%-7s kept %.1f%% of the working set through a %dx scan", name, survival[name]*100, scanKeys/hotKeys)
	}

	if survival["s3fifo"] <= survival["lru"] {
		t.Errorf("s3fifo kept %.3f of the working set, lru kept %.3f: the probationary queue is not doing its job",
			survival["s3fifo"], survival["lru"])
	}
}

func BenchmarkGetHit(b *testing.B) {
	c, err := New(Config{
		NewPolicy:     func(int64) Policy { return NewLRU(5) },
		CapacityBytes: 64 << 20,
		Shards:        256,
	})
	if err != nil {
		b.Fatal(err)
	}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
		c.Put(keys[i], make([]byte, 512))
	}

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, ok := c.Get(keys[i&1023]); !ok {
				b.Fatal("unexpected miss")
			}
			i++
		}
	})
}

func BenchmarkEvict(b *testing.B) {
	for name, mk := range policies {
		b.Run(name, func(b *testing.B) {
			// One shard sized so that every insert forces an eviction.
			c, err := New(Config{
				NewPolicy:     mk,
				CapacityBytes: 512 << 10,
				Shards:        1,
			})
			if err != nil {
				b.Fatal(err)
			}
			value := make([]byte, 512)
			for i := range 2048 {
				c.Put(fmt.Sprintf("warm-%d", i), value)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				c.Admit(fmt.Sprintf("k-%d", i), value)
			}
			b.ReportMetric(float64(c.Snapshot().EvictNSMean), "ns/victim")
		})
	}
}
