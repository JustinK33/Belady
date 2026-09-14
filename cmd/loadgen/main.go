// Command loadgen replays a synthetic workload against a running cluster and
// reports what happened, including how far the cache is from the offline optimum.
//
// It is a measurement tool, not a service: it generates the trace up front so the
// exact same request sequence can be scored by Belady's MIN afterwards. A
// generated-on-the-fly workload would be cheaper and would make the comparison
// meaningless.
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/belady"
	"github.com/JustinK33/Belady/internal/cache"
	"github.com/JustinK33/Belady/internal/config"
	"github.com/JustinK33/Belady/internal/grpcx"
)

type options struct {
	target      string
	requests    int
	keyspace    int
	concurrency int
	warmup      int
	zipfS       float64

	// Optional thresholds. Zero means no gate, which is what you want locally; CI
	// sets them so a policy or hot-path regression fails the build instead of
	// scrolling past in the report.
	minObjectHit float64
	maxP99       time.Duration
}

func main() {
	opts := options{
		target:      config.String("TARGET_ADDR", "localhost:8080"),
		requests:    config.Int("REQUESTS", 200_000),
		keyspace:    config.Int("KEYSPACE", 100_000),
		concurrency: config.Int("CONCURRENCY", 64),
		warmup:      config.Int("WARMUP", 20_000),
		zipfS:       config.Float("ZIPF_S", 1.1),

		minObjectHit: config.Float("MIN_OBJECT_HIT", 0),
		maxP99:       config.Duration("MAX_P99", 0),
	}
	if opts.zipfS <= 1 {
		fmt.Fprintln(os.Stderr, "ZIPF_S must be greater than 1")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := grpcx.Dial(opts.target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", opts.target, err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()
	client := beladyv1.NewCacheClient(conn)

	trace := generateTrace(opts)

	// Warmup requests are excluded from the reported numbers. Measuring a cold
	// cache measures how fast the origin is, not how good the policy is.
	if opts.warmup > 0 {
		fmt.Printf("warming up with %d requests\n", opts.warmup)
		if _, err := replay(ctx, client, trace[:min(opts.warmup, len(trace))], opts.concurrency); err != nil {
			fmt.Fprintf(os.Stderr, "warmup: %v\n", err)
			os.Exit(1)
		}
	}

	before, err := client.Stats(ctx, &beladyv1.StatsRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("replaying %d requests at concurrency %d\n", len(trace), opts.concurrency)
	start := time.Now()
	res, err := replay(ctx, client, trace, opts.concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay: %v\n", err)
		os.Exit(1)
	}
	elapsed := time.Since(start)

	after, err := client.Stats(ctx, &beladyv1.StatsRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats: %v\n", err)
		os.Exit(1)
	}

	sd := delta(before, after)
	report(opts, trace, res, sd, elapsed)

	if !withinThresholds(opts, res, sd) {
		os.Exit(1)
	}
}

// withinThresholds is the perf gate. It lives here rather than in a Go test because
// it needs a running cluster, and it reports every violation rather than the first,
// so one CI run tells you whether the hit ratio moved, the tail moved, or both.
func withinThresholds(opts options, res *results, sd serverDelta) bool {
	ok := true
	if opts.minObjectHit > 0 {
		if got := ratio(sd.hits, sd.hits+sd.misses); got < opts.minObjectHit {
			fmt.Fprintf(os.Stderr, "FAIL object hit %.4f is below the MIN_OBJECT_HIT floor of %.4f\n", got, opts.minObjectHit)
			ok = false
		}
	}
	if opts.maxP99 > 0 {
		if got := res.quantile(0.99); got > opts.maxP99 {
			fmt.Fprintf(os.Stderr, "FAIL p99 %s is above the MAX_P99 ceiling of %s\n", got, opts.maxP99)
			ok = false
		}
	}
	return ok
}

// generateTrace draws keys from a Zipf distribution. Web popularity is
// consistently Zipf-like with an exponent near 1; a uniform workload would make
// every policy look identical, because with no skew there is nothing to predict.
func generateTrace(opts options) []string {
	// A fixed seed by default: comparing two policies means replaying the same
	// trace, not two draws from the same distribution.
	rng := rand.New(rand.NewPCG(uint64(config.Int("SEED", 1)), 0x9e3779b97f4a7c15))
	zipf := rand.NewZipf(rng, opts.zipfS, 1, uint64(opts.keyspace-1))

	trace := make([]string, opts.requests)
	for i := range trace {
		trace[i] = "key-" + strconv.FormatUint(zipf.Uint64(), 10)
	}
	return trace
}

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
			}
		}
	}
	sort.Slice(res.latencies, func(i, j int) bool { return res.latencies[i] < res.latencies[j] })
	return res, nil
}

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

func report(opts options, trace []string, res *results, sd serverDelta, elapsed time.Duration) {
	objectHit := ratio(sd.hits, sd.hits+sd.misses)
	byteHit := ratio(sd.hitB, sd.missB+sd.hitB)

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

	// The optimum is scored on the same trace the server just replayed, at a
	// capacity converted from bytes using the mean object size actually observed.
	if res.served > 0 && sd.objects > 0 {
		meanSize := float64(sd.bytesUsed) / float64(sd.objects)
		capacityObjects := int(float64(sd.bytesCapacity) / meanSize)

		// MIN has to see the warmup too. The server entered the measured window with
		// a populated cache; scoring the optimum from cold would compare a warm
		// policy against a cold ceiling and the ceiling can land below the policy.
		warmup := min(opts.warmup, len(trace))
		scored := make([]string, 0, warmup+len(trace))
		scored = append(scored, trace[:warmup]...)
		scored = append(scored, trace...)
		optimum := belady.MINHitRatioFrom(hashTrace(scored), capacityObjects, warmup)

		fmt.Printf("\noffline optimum (Belady MIN)\n")
		row("capacity", fmt.Sprintf("%d objects, from a %.0f byte mean", capacityObjects, meanSize))
		row("object hit", fmt.Sprintf("%.4f", optimum))
		row("gap", fmt.Sprintf("%+.2f points", (objectHit-optimum)*100))
		// Two reasons this is a bound and not an equality: capacity is counted in
		// objects rather than bytes, and MIN models one unified cache while the real
		// cluster is nodes x shards, each independently capped. Sharding can only
		// lose hits, so a real policy scoring above MIN means the conversion is off,
		// not that the policy beat optimal.
		row("caveat", "count-based and unsharded, so an estimate of the ceiling")
	}

	fmt.Printf("\nclient latency\n")
	row("p50", res.quantile(0.50).String())
	row("p90", res.quantile(0.90).String())
	row("p99", res.quantile(0.99).String())
	row("p99.9", res.quantile(0.999).String())
	row("max", res.quantile(1).String())
	row("throughput", fmt.Sprintf("%.0f req/s, %s/s", float64(res.served)/elapsed.Seconds(), bytes(uint64(float64(res.bytes)/elapsed.Seconds()))))
	fmt.Println()
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

// hashTrace converts keys to the same 64-bit identity the cache uses, so the
// optimum is scored on exactly the objects the server saw.
func hashTrace(trace []string) []uint64 {
	out := make([]uint64, len(trace))
	for i, k := range trace {
		out[i] = cache.Hash(k)
	}
	return out
}

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
