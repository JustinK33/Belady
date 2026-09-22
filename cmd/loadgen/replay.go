package main

import (
	"context"
	"sort"
	"sync"
	"time"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
)

type sample struct {
	latency   time.Duration
	bytes     int
	fromCache bool
}

type results struct {
	latencies []time.Duration
	served    int
	fromCache int
	bytes     int64

	// Latency totals split by whether the gateway answered from cache, which is what
	// makes the marginal cost of a miss a measured quantity rather than an assumed
	// one. It is not the configured ORIGIN_LATENCY: a miss also pays gRPC out to the
	// origin and back.
	hitNS, missNS int64
}

func replay(ctx context.Context, client beladyv1.CacheClient, trace []string, concurrency int) (*results, error) {
	type slot struct {
		err     error
		samples []sample
	}
	slots := make([]slot, concurrency)

	var wg sync.WaitGroup
	for w := range concurrency {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Each worker takes a strided slice of the trace. Striding rather than
			// splitting into blocks keeps every worker's key popularity distribution
			// the same as the whole trace's.
			out := make([]sample, 0, len(trace)/concurrency+1)
			for i := w; i < len(trace); i += concurrency {
				if ctx.Err() != nil {
					break
				}
				t0 := time.Now()
				resp, err := client.Get(ctx, &beladyv1.GetRequest{Key: trace[i]})
				if err != nil {
					slots[w].err = err
					break
				}
				out = append(out, sample{
					latency:   time.Since(t0),
					bytes:     len(resp.GetValue()),
					fromCache: resp.GetSource() == beladyv1.Source_SOURCE_CACHE,
				})
			}
			slots[w].samples = out
		}(w)
	}
	wg.Wait()

	res := &results{latencies: make([]time.Duration, 0, len(trace))}
	for _, s := range slots {
		if s.err != nil {
			return nil, s.err
		}
		for _, sm := range s.samples {
			res.latencies = append(res.latencies, sm.latency)
			res.served++
			res.bytes += int64(sm.bytes)
			if sm.fromCache {
				res.fromCache++
				res.hitNS += sm.latency.Nanoseconds()
			} else {
				res.missNS += sm.latency.Nanoseconds()
			}
		}
	}
	sort.Slice(res.latencies, func(i, j int) bool { return res.latencies[i] < res.latencies[j] })
	return res, nil
}

// meanMiss minus meanHit is the marginal cost of a miss, which is the quantity the
// break-even arithmetic in docs/03-performance.md is expressed in. Reporting it
// beside the percentiles is what turns "is a learned policy worth it here" into a
// comparison against a number rather than against the origin's configured latency.
//
// ponytail: mean-field, not causal. At concurrency 64 a slow miss also delays the
// hits queued behind it, so part of the miss cost is charged to the hits and the
// difference understates one miss in isolation. The upgrade is an open-loop
// generator at a fixed arrival rate, which removes the coupling; until then the
// check that it is credible is that the difference tracks ORIGIN_LATENCY with a
// slope near 1.
func (r *results) mean() time.Duration     { return meanOf(r.hitNS+r.missNS, r.served) }
func (r *results) meanHit() time.Duration  { return meanOf(r.hitNS, r.fromCache) }
func (r *results) meanMiss() time.Duration { return meanOf(r.missNS, r.served-r.fromCache) }

func meanOf(total int64, n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return (time.Duration(total) / time.Duration(n)).Round(time.Microsecond)
}

// quantile reads straight off the sorted samples. Every latency is kept rather
// than bucketed, which costs 8 bytes per request and gives exact percentiles
// instead of the nearest histogram bucket.
func (r *results) quantile(q float64) time.Duration {
	if len(r.latencies) == 0 {
		return 0
	}
	i := int(q * float64(len(r.latencies)-1))
	return r.latencies[i].Round(time.Microsecond)
}
