// Command loadgen replays a synthetic workload against a running cluster and
// reports what happened, including how far the cache is from the offline optimum.
//
// It is a measurement tool, not a service: it generates the trace up front so the
// exact same request sequence can be scored by Belady's MIN afterwards. A
// generated-on-the-fly workload would be cheaper and would make the comparison
// meaningless.
//
// The package is split by responsibility: workload.go draws the trace, replay.go
// issues it and collects latencies, stats.go turns two server snapshots into the
// window's counters, and report.go renders either form of output. This file is the
// sequencing and the perf gate, and nothing else.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
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
		// Progress goes to stderr, because with REPORT_JSON on stdout is one object
		// and a sweep pipes it straight into jq.
		fmt.Fprintf(os.Stderr, "warming up with %d requests\n", opts.warmup)
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

	fmt.Fprintf(os.Stderr, "replaying %d requests at concurrency %d\n", len(trace), opts.concurrency)
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
	d := derive(opts, trace, res, sd)
	if config.Bool("REPORT_JSON", false) {
		reportJSON(opts, res, sd, d, elapsed)
	} else {
		report(opts, res, sd, d, elapsed)
	}

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
