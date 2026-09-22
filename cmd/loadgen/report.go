package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/JustinK33/Belady/internal/config"
)

func report(opts options, res *results, sd serverDelta, d derived, elapsed time.Duration) {
	objectHit, byteHit := d.objectHit, d.byteHit

	fmt.Printf("\nbelady loadgen\n")
	row("target", opts.target)
	row("requests", fmt.Sprintf("%d served in %s at concurrency %d", res.served, elapsed.Round(time.Millisecond), opts.concurrency))
	row("workload", fmt.Sprintf("zipf s=%.2f over %d keys", opts.zipfS, opts.keyspace))

	fmt.Printf("\nserver\n")
	row("policy", sd.policy+versionSuffix(sd.modelVersion))
	row("capacity", fmt.Sprintf("%s, %s used across %d objects", bytes(sd.bytesCapacity), bytes(sd.bytesUsed), sd.objects))
	row("object hit", fmt.Sprintf("%.4f", objectHit))
	row("byte hit", fmt.Sprintf("%.4f", byteHit))
	// The client sees which node answered from cache. If this disagrees with the
	// server's own count, routing or stats aggregation is broken, not the policy.
	row("client view", fmt.Sprintf("%.4f served from cache", ratio(uint64(res.fromCache), uint64(res.served))))
	row("evictions", fmt.Sprintf("%d, %d rejected", sd.evictions, sd.rejections))
	row("evict cost", fmt.Sprintf("%d ns/victim over the window (includes one clock read)", sd.evictNS))

	if d.haveOptimum {
		fmt.Printf("\noffline optimum (Belady MIN)\n")
		row("capacity", fmt.Sprintf("%d objects, from a %.0f byte mean", d.capacityObjects, d.meanSize))
		row("object hit", fmt.Sprintf("%.4f", d.optimum))
		row("gap", fmt.Sprintf("%+.2f points", (objectHit-d.optimum)*100))
		// Two reasons this is a bound and not an equality: capacity is counted in
		// objects rather than bytes, and MIN models one unified cache while the real
		// cluster is nodes x shards, each independently capped. Sharding can only
		// lose hits, so a real policy scoring above MIN means the conversion is off,
		// not that the policy beat optimal.
		row("caveat", "count-based and unsharded, so an estimate of the ceiling")
	}

	fmt.Printf("\nclient latency\n")
	row("mean", fmt.Sprintf("%s (hit %s, miss %s, marginal %s)",
		res.mean(), res.meanHit(), res.meanMiss(), res.meanMiss()-res.meanHit()))
	row("p50", res.quantile(0.50).String())
	row("p90", res.quantile(0.90).String())
	row("p99", res.quantile(0.99).String())
	row("p99.9", res.quantile(0.999).String())
	row("max", res.quantile(1).String())
	row("throughput", fmt.Sprintf("%.0f req/s, %s/s", float64(res.served)/elapsed.Seconds(), bytes(uint64(float64(res.bytes)/elapsed.Seconds()))))
	fmt.Println()
}

// record is the sweep's interface to this tool. The text report above stays the
// default and is what docs/03-performance.md quotes; an origin-latency sweep is
// thirty runs, and scraping that text would be the most fragile part of the
// measurement.
//
// Flat, snake_case and indented, following the trainer's Result.summary(), which is
// the only other machine-readable stdout in the tree.
type record struct {
	Policy       string `json:"policy"`
	ModelVersion string `json:"model_version"`
	// OriginLatency is echoed straight from the environment, so a row of JSONL carries
	// its own x axis. It is the one field here loadgen cannot verify: it is what the
	// sweep asked the origin for, and the check that the origin honoured it is that
	// marginal_miss_ns tracks it with a slope near 1.
	OriginLatency string `json:"origin_latency"`

	Requests    int     `json:"requests"`
	Concurrency int     `json:"concurrency"`
	Keyspace    int     `json:"keyspace"`
	ZipfS       float64 `json:"zipf_s"`

	ObjectHit float64 `json:"object_hit"`
	ByteHit   float64 `json:"byte_hit"`
	// ClientHit is the same quantity counted on the other side of the wire: the
	// fraction of responses the gateway marked as served from cache, rather than the
	// nodes' own shard counters. The text report cross-checks these two; carrying it
	// in the JSON is what lets a sweep do the same without re-deriving it from the
	// hit and miss means. They disagree when routing or stats aggregation is broken,
	// which is a class of bug that leaves the hit ratio looking entirely plausible.
	ClientHit float64 `json:"client_hit"`

	BeladyMIN           float64 `json:"belady_min"`
	Evictions           uint64  `json:"evictions"`
	Rejections          uint64  `json:"rejections"`
	EvictionsPerRequest float64 `json:"evictions_per_request"`
	EvictNSPerVictim    uint64  `json:"evict_ns_per_victim"`
	BytesUsed           uint64  `json:"bytes_used"`

	MeanNS         int64 `json:"mean_ns"`
	MeanHitNS      int64 `json:"mean_hit_ns"`
	MeanMissNS     int64 `json:"mean_miss_ns"`
	MarginalMissNS int64 `json:"marginal_miss_ns"`
	P50NS          int64 `json:"p50_ns"`
	P99NS          int64 `json:"p99_ns"`
	P999NS         int64 `json:"p99_9_ns"`

	ReqPerSec float64 `json:"req_per_sec"`
	ElapsedMS int64   `json:"elapsed_ms"`
}

func reportJSON(opts options, res *results, sd serverDelta, d derived, elapsed time.Duration) {
	rec := record{
		Policy:        sd.policy,
		ModelVersion:  sd.modelVersion,
		OriginLatency: config.String("ORIGIN_LATENCY", ""),

		Requests:    res.served,
		Concurrency: opts.concurrency,
		Keyspace:    opts.keyspace,
		ZipfS:       opts.zipfS,

		ObjectHit:           round4(d.objectHit),
		ByteHit:             round4(d.byteHit),
		ClientHit:           round4(ratio(uint64(res.fromCache), uint64(res.served))),
		BeladyMIN:           round4(d.optimum),
		Evictions:           sd.evictions,
		Rejections:          sd.rejections,
		EvictionsPerRequest: round4(d.evictionsPerRequest),
		EvictNSPerVictim:    sd.evictNS,
		BytesUsed:           sd.bytesUsed,

		MeanNS:         res.mean().Nanoseconds(),
		MeanHitNS:      res.meanHit().Nanoseconds(),
		MeanMissNS:     res.meanMiss().Nanoseconds(),
		MarginalMissNS: (res.meanMiss() - res.meanHit()).Nanoseconds(),
		P50NS:          res.quantile(0.50).Nanoseconds(),
		P99NS:          res.quantile(0.99).Nanoseconds(),
		P999NS:         res.quantile(0.999).Nanoseconds(),

		ReqPerSec: round4(float64(res.served) / elapsed.Seconds()),
		ElapsedMS: elapsed.Milliseconds(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
		os.Exit(1)
	}
}

// round4 keeps the ratios readable at the resolution they are trustworthy to. Hit
// ratio repeats to a few basis points run to run, so a fifth decimal place would be
// noise dressed as precision.
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func row(label, value string) { fmt.Printf("  %-12s %s\n", label, value) }

func versionSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " (model " + v + ")"
}

func ratio(num, den uint64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func bytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", v)
}
