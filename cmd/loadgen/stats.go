package main

import (
	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/belady"
)

// serverDelta is the change in server counters across the measured window, so a
// warmup does not drag the reported hit ratio up.
type serverDelta struct {
	policy        string
	modelVersion  string
	hits, misses  uint64
	hitB, missB   uint64
	evictions     uint64
	rejections    uint64
	objects       uint64
	bytesUsed     uint64
	bytesCapacity uint64

	// evictNS is the mean cost of a victim choice over the measured window, not the
	// server's lifetime mean. The lifetime mean carries every warmup eviction
	// forever, and warmup is where a cold cache pays its worst ones, so quoting it
	// in a benchmark overstates the policy's cost by an amount that depends on how
	// long the warmup was.
	evictNS uint64
}

func delta(before, after *beladyv1.StatsResponse) serverDelta {
	// Recomputed from the summed pair rather than differenced, because a mean is not
	// a counter: after-minus-before on evict_ns_mean is meaningless.
	var evictNS uint64
	if samples := after.GetEvictSample() - before.GetEvictSample(); samples > 0 {
		evictNS = (after.GetEvictNs() - before.GetEvictNs()) / samples
	}
	return serverDelta{
		policy:        after.GetPolicy(),
		modelVersion:  after.GetModelVersion(),
		hits:          after.GetHits() - before.GetHits(),
		misses:        after.GetMisses() - before.GetMisses(),
		hitB:          after.GetHitBytes() - before.GetHitBytes(),
		missB:         after.GetMissBytes() - before.GetMissBytes(),
		evictions:     after.GetEvictions() - before.GetEvictions(),
		rejections:    after.GetRejections() - before.GetRejections(),
		objects:       after.GetObjects(),
		bytesUsed:     after.GetBytesUsed(),
		bytesCapacity: after.GetBytesCapacity(),
		evictNS:       evictNS,
	}
}

// derived is everything computed from a run rather than counted during it. It is
// hoisted out of the reporters so the text and the JSON forms cannot come to
// different conclusions about the object-count conversion behind Belady's MIN.
type derived struct {
	objectHit, byteHit  float64
	evictionsPerRequest float64

	// haveOptimum is false when nothing was served or the cache reported no objects,
	// because then there is no observed mean object size to convert a byte capacity
	// with and the MIN comparison has no denominator.
	haveOptimum     bool
	meanSize        float64
	capacityObjects int
	optimum         float64
}

func derive(opts options, trace []string, res *results, sd serverDelta) derived {
	d := derived{
		objectHit: ratio(sd.hits, sd.hits+sd.misses),
		byteHit:   ratio(sd.hitB, sd.missB+sd.hitB),
	}
	if res.served == 0 {
		return d
	}
	// Evictions per request is what converts a per-victim cost into a per-request one,
	// which is the only form in which it can be set against a hit-ratio gain.
	d.evictionsPerRequest = float64(sd.evictions) / float64(res.served)
	if sd.objects == 0 {
		return d
	}

	// The optimum is scored on the same trace the server just replayed, at a capacity
	// converted from bytes using the mean object size actually observed.
	d.haveOptimum = true
	d.meanSize = float64(sd.bytesUsed) / float64(sd.objects)
	d.capacityObjects = int(float64(sd.bytesCapacity) / d.meanSize)

	// MIN has to see the warmup too. The server entered the measured window with a
	// populated cache; scoring the optimum from cold would compare a warm policy
	// against a cold ceiling and the ceiling can land below the policy.
	warmup := min(opts.warmup, len(trace))
	scored := make([]string, 0, warmup+len(trace))
	scored = append(scored, trace[:warmup]...)
	scored = append(scored, trace...)
	d.optimum = belady.MINHitRatioFrom(hashTrace(scored), d.capacityObjects, warmup)
	return d
}
