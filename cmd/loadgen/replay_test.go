package main

import (
	"testing"
	"time"
)

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
