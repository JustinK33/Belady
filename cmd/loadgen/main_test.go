package main

import (
	"fmt"
	"testing"
	"time"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
)

// TestDeltaRecomputesTheEvictionMeanOverTheWindow pins the one field in delta that is
// not a subtraction. evict_ns_mean is a lifetime mean, so differencing it would be
// arithmetic on the wrong kind of number and would still produce a plausible-looking
// nanosecond figure, which is how a wrong eviction cost gets published.
func TestDeltaRecomputesTheEvictionMeanOverTheWindow(t *testing.T) {
	before := &beladyv1.StatsResponse{
		Hits: 100, Misses: 50, HitBytes: 1000, MissBytes: 500,
		Evictions: 10, Rejections: 1,
		// Warmup was slow: 40 victims at 5000 ns each.
		EvictNs: 200_000, EvictSample: 40, EvictNsMean: 5000,
	}
	after := &beladyv1.StatsResponse{
		Policy: "lrb", ModelVersion: "v3",
		Hits: 900, Misses: 150, HitBytes: 9000, MissBytes: 1500,
		Evictions: 30, Rejections: 3,
		Objects: 4096, BytesUsed: 6 << 20, BytesCapacity: 8 << 20,
		// 60 more victims at 1000 ns each, so the lifetime mean is still 2600.
		EvictNs: 260_000, EvictSample: 100, EvictNsMean: 2600,
	}

	sd := delta(before, after)
	if sd.evictNS != 1000 {
		t.Errorf("evictNS = %d, want 1000: the window mean, not %d lifetime or a difference of means",
			sd.evictNS, after.GetEvictNsMean())
	}
	if sd.hits != 800 || sd.misses != 100 || sd.hitB != 8000 || sd.missB != 1000 {
		t.Errorf("counters not differenced: %+v", sd)
	}
	if sd.evictions != 20 || sd.rejections != 2 {
		t.Errorf("evictions = %d, rejections = %d; want 20, 2", sd.evictions, sd.rejections)
	}
	// Occupancy is a gauge, so it comes from after untouched rather than differenced.
	if sd.objects != 4096 || sd.bytesUsed != 6<<20 || sd.bytesCapacity != 8<<20 {
		t.Errorf("gauges were differenced: %+v", sd)
	}
	if sd.policy != "lrb" || sd.modelVersion != "v3" {
		t.Errorf("policy = %q, model = %q", sd.policy, sd.modelVersion)
	}

	// A window with no evictions divides by zero if the guard is dropped.
	if got := delta(before, &beladyv1.StatsResponse{EvictNs: 200_000, EvictSample: 40}).evictNS; got != 0 {
		t.Errorf("evictNS = %d over a window with no victims, want 0", got)
	}
}

// TestDeriveConvertsByteCapacityToObjects covers the conversion behind the "gap to
// Belady MIN" column. MIN is count-based and the cache is byte-based, so the ceiling
// depends on a mean object size measured at the end of the run. Getting it wrong is a
// documented failure mode: the ceiling lands below the policy it is meant to bound.
func TestDeriveConvertsByteCapacityToObjects(t *testing.T) {
	// Five distinct keys cycled four times, and a cache sized for exactly five: MIN
	// pays the five compulsory misses and nothing else.
	trace := make([]string, 0, 20)
	for range 4 {
		for k := range 5 {
			trace = append(trace, fmt.Sprintf("k%d", k))
		}
	}
	sd := serverDelta{
		hits: 750, misses: 250, hitB: 7500, missB: 2500,
		evictions: 100,
		objects:   10, bytesUsed: 1000, bytesCapacity: 500,
	}
	res := &results{served: 1000}

	d := derive(options{}, trace, res, sd)
	if d.objectHit != 0.75 || d.byteHit != 0.75 {
		t.Errorf("objectHit = %v, byteHit = %v; want 0.75, 0.75", d.objectHit, d.byteHit)
	}
	if d.evictionsPerRequest != 0.1 {
		t.Errorf("evictionsPerRequest = %v, want 0.1", d.evictionsPerRequest)
	}
	if d.meanSize != 100 {
		t.Errorf("meanSize = %v, want 100", d.meanSize)
	}
	if d.capacityObjects != 5 {
		t.Errorf("capacityObjects = %v, want 5: 500 bytes of capacity at 100 bytes an object", d.capacityObjects)
	}
	if !d.haveOptimum {
		t.Fatal("haveOptimum = false with a populated cache")
	}
	if want := 1 - 5.0/20.0; d.optimum != want {
		t.Errorf("optimum = %v, want %v from cold: 5 compulsory misses in 20 requests", d.optimum, want)
	}

	// The warmup prefix is what keeps the ceiling above a warm policy: replay the
	// trace once first and MIN pays no compulsory misses inside the scored window.
	// A warmup longer than the trace is clamped rather than slicing out of range.
	for _, warmup := range []int{len(trace), 1000} {
		if got := derive(options{warmup: warmup}, trace, res, sd).optimum; got != 1 {
			t.Errorf("optimum = %v at warmup %d, want 1: the working set fits", got, warmup)
		}
	}

	// Nothing served, and an empty cache, each leave the optimum undefined rather
	// than dividing by zero into a NaN that formats as a real number.
	if d := derive(options{}, trace, &results{}, sd); d.haveOptimum || d.evictionsPerRequest != 0 {
		t.Errorf("nothing served: haveOptimum = %v, evictionsPerRequest = %v", d.haveOptimum, d.evictionsPerRequest)
	}
	empty := sd
	empty.objects = 0
	if d := derive(options{}, trace, res, empty); d.haveOptimum {
		t.Error("haveOptimum = true with an empty cache")
	}
}

// TestMeansSplitByHitAndMiss covers the one piece of arithmetic behind the marginal
// miss cost. The failure it is really guarding against is a zero denominator: an
// all-hit run has no misses, and a NaN or a divide-by-zero panic there would take
// out every row of a sweep rather than one.
func TestMeansSplitByHitAndMiss(t *testing.T) {
	collect := func(samples ...sample) *results {
		r := &results{}
		for _, s := range samples {
			r.served++
			if s.fromCache {
				r.fromCache++
				r.hitNS += s.latency.Nanoseconds()
			} else {
				r.missNS += s.latency.Nanoseconds()
			}
		}
		return r
	}
	hit := func(d time.Duration) sample { return sample{latency: d, fromCache: true} }
	miss := func(d time.Duration) sample { return sample{latency: d} }

	cases := []struct {
		name               string
		res                *results
		mean, hits, misses time.Duration
	}{
		{
			name: "mixed",
			// Three hits averaging 200µs, one miss at 2ms: overall (600+2000)/4 = 650µs.
			res:    collect(hit(100*time.Microsecond), hit(200*time.Microsecond), hit(300*time.Microsecond), miss(2*time.Millisecond)),
			mean:   650 * time.Microsecond,
			hits:   200 * time.Microsecond,
			misses: 2 * time.Millisecond,
		},
		{
			name:   "all hits",
			res:    collect(hit(50*time.Microsecond), hit(150*time.Microsecond)),
			mean:   100 * time.Microsecond,
			hits:   100 * time.Microsecond,
			misses: 0,
		},
		{
			name:   "all misses",
			res:    collect(miss(time.Millisecond), miss(3*time.Millisecond)),
			mean:   2 * time.Millisecond,
			hits:   0,
			misses: 2 * time.Millisecond,
		},
		{name: "nothing served", res: collect()},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.res.mean(); got != c.mean {
				t.Errorf("mean = %s, want %s", got, c.mean)
			}
			if got := c.res.meanHit(); got != c.hits {
				t.Errorf("meanHit = %s, want %s", got, c.hits)
			}
			if got := c.res.meanMiss(); got != c.misses {
				t.Errorf("meanMiss = %s, want %s", got, c.misses)
			}
		})
	}
}
