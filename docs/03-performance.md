# Performance

## The budget, and why there is one

The model runs inside the eviction path, under a shard mutex, on a miss.
That is the constraint the whole project is built around, and it decomposes like this:

| Path | Budget | Why |
| --- | --- | --- |
| Cache hit | tens of nanoseconds | A hit is a hash, a mutex, a map lookup and a history update. Anything more and the cache is slower than the thing it fronts. |
| Eviction, whole | under 1 µs | An eviction is amortised over roughly one request in eight at these hit ratios. A microsecond there is ~0.1 µs per request, which is invisible against any real origin. |
| Model evaluation, per candidate | ~100 ns | The eviction budget divided by `CACHE_SAMPLE_SIZE`, which is 8. |
| Trace push | under 20 ns | It happens on every sampled request, hit or miss. |

These are the numbers the design decisions below are trying to hit, not measurements.
The measurements are further down and one of them misses.

## Where the allocations went

Zero allocations on the hit path and zero in the eviction loop, both asserted by benchmarks with `-benchmem`.

**The store.** `internal/cache` is `2^k` independently locked shards, chosen by the low bits of the key hash.
No cache-wide lock and no cache-wide counter: every counter lives on the shard, guarded by the mutex the caller is already holding.
`Snapshot()` sums them, and that runs on a Prometheus scrape, not on a request.

The shard lock is a plain `sync.Mutex`, not an `RWMutex`.
A hit mutates the entry's access history, so a reader needs exclusive access anyway, and `RWMutex` costs more than `Mutex` when every acquisition is a write.

**Values.** Individually allocated `[]byte`, handed out without copying.
An arena would cut GC pressure, but grpc-go's codec copies the value into the response message regardless, so an arena would buy allocator wins only, in exchange for a use-after-free class of bug on eviction.
The marker is on `Entry` in `internal/cache/entry.go`; the trigger for revisiting is pprof showing GC dominating.
This is [ADR 0006](adr/0006-in-memory-values-on-the-go-heap.md).

**The delta history.** A fixed `[8]uint32` on the entry, shifted with one `copy` on each access.
A ring buffer would avoid the copy, but 32 bytes is a single cache line and the index arithmetic would then be paid back on every feature extraction instead, which happens eight times per eviction rather than once per access.

**The eviction loop.** The candidate visitor is a `func(*Entry) bool` stored on the policy struct at construction, and the running best is kept in struct fields.
Both exist so no closure is allocated per eviction under the shard lock.
The feature vector is one reused `[]float32` and the `features.Input` is one reused struct, filled by pointer.

**The model.** One contiguous `[]node` of 16-byte nodes, plus a `[]int32` of tree roots.
No pointers, no interface dispatch, no per-tree slice header.
A leaf is encoded as a negative index (`^v`), so the inner loop is a compare, a load and a branch with no separate leaf array to chase.
This is [ADR 0005](adr/0005-hand-rolled-evaluator.md).

**The trace ring.** A lock-free single-producer, single-consumer ring of 24-byte pointer-free records, one ring per shard.
Producer and consumer cursors are separated by 64 bytes of padding so they never share a cache line, and the consumer's cached copy of the head is refreshed only when the ring looks full.
Publication is a release-store on the tail.

The ring is only correct with one producer, and the producer is the shard lock holder, which is what makes that true by construction rather than by convention.
Because the shard count is rounded up to a power of two, an operator asking for 100 shards gets 128, and a recorder built for 100 rings would hand two shards the same ring and corrupt the trace with no symptom at all.
`cache.New` therefore refuses to start when `cfg.Trace.Shards()` disagrees with the rounded shard count.

**Sampling by key hash, not by request.** Relaxed Belady labelling needs the complete access history of the keys it labels.
Sampling one request in sixteen produces histories full of holes where every delta is wrong.
Sampling on `(key_hash >> 32) & mask` keeps every sampled key's history complete.
The high bits, because the low bits already select the shard.
The cost is a sampled fraction that swings run to run depending on whether the hottest keys landed in the sample.

**gRPC.** `MaxRecvMsgSize` 8 MiB, `MaxConcurrentStreams` 4096, keepalive pinging every 30 s with a 10 s timeout and a 10 s minimum enforced on clients.
A chained unary interceptor recovers panics and records `belady_rpc_duration_seconds{method,code}`.
Latency buckets are `ExponentialBuckets(50e-6, 2, 19)`, which is 50 µs doubling to about 13 s: wide enough that an origin timeout still lands in a bucket rather than in `+Inf`.

## Microbenchmarks

Run with `make bench-micro`, which is `go test ./internal/... -run '^$' -bench . -benchmem`.
Apple M4, `darwin/arm64`, 2026-09-09.

| Benchmark | Result | Allocations |
| --- | --- | --- |
| `cache/GetHit` (parallel, 256 shards) | 22.77 ns/op | 0 |
| `trace/RingPush` | 13.44 ns/op | 0 |
| `cache/Evict/s3fifo` | 79 ns/victim | 0 in the policy |
| `cache/Evict/lfu` | 173 ns/victim | 0 in the policy |
| `cache/Evict/lru` | 195 ns/victim | 0 in the policy |
| `cache/Evict/lrb` | 254 ns/victim | 0 in the policy |
| `cache/LRBVictim` | 170.2 ns/op | 0 |
| `hashring/Pick` (parallel, 8 nodes, 256 replicas) | 341.5 ns/op | 0 |
| `model/Raw` 50 trees, 32 leaves | 183.2 ns/op, 1465 ns/eviction | 0 |
| `model/Raw` 100 trees, 64 leaves | 488.3 ns/op, 3906 ns/eviction | 0 |
| `model/Raw` 200 trees, 64 leaves | 1014 ns/op, 8111 ns/eviction | 0 |
| `model/Load` 100 trees | 1.32 ms/op, 253 MB/s | 2346 |
| `belady/MINHitRatio` 200k requests, 100k keys, 10k capacity | 11.8 ms/op | 208k |

The 3 allocations per op reported by `Evict/*` are the benchmark's own `fmt.Sprintf` and the value slice, not the policy.
`model/Load` allocates freely on purpose: it runs off the request path, once per model version, and 1.3 ms to parse a 100-tree dump is not a number worth optimising.

`Evict/*` reports the store's own `evict_ns_mean` counter, which times only `policy.Victim` plus one clock read.
It is the same counter the running cluster exports, which is what makes the microbenchmark and the live number directly comparable.

### Reading the model numbers

The `ns/eviction` column on `model/Raw` is the per-evaluation cost times 8, the default sample size.
A 50-tree, 32-leaf model therefore costs about 1.5 µs per eviction in evaluation alone, which is over the budget.
The model this project actually trains is 31 trees at `num_leaves=32`, and scaling linearly on tree count puts it at roughly 113 ns per candidate and **about 0.9 µs per eviction**.

So the sub-microsecond budget holds for the model that gets trained, with almost no headroom, and it is exceeded somewhere above 35 trees.
That is a real constraint on the trainer, not a theoretical one: `MAX_ROUNDS` is 300 with early stopping at 25, and the only reason the fitted model is small is that early stopping fires.
A workload that needed 200 trees would need either a smaller `CACHE_SAMPLE_SIZE` or a different story about the budget.

## Measured cluster numbers

Most recent full pass, on the configuration recorded here.
The pass in [ROADMAP.md](../ROADMAP.md) step 16 will re-run this from a clean state.

**Setup.**
Three cache nodes, 32 shards each, 6 MiB per node.
Zipfian `s = 1.1` over 50,000 keys, 400,000 requests at concurrency 64, 20,000-request warmup excluded from the numbers.
Pareto object sizes with a 4 KiB mean, 200 µs origin latency.
Apple M4, native, not in Docker.
Each policy starts cold.
The model is a 31-tree LightGBM fit on 217,283 rows sampled from an 8.5 s trace of the same workload, holdout AUC 0.985.

| Policy | Object hit | Byte hit | Gap to Belady MIN | Evict cost | p99 | p99.9 | Throughput |
| --- | --- | --- | --- | --- | --- | --- | --- |
| LRU (sampled, 8) | 0.8732 | 0.8645 | -6.96 pts | 815 ns/victim | 3.31 ms | 4.55 ms | 51,967 req/s |
| Learned (LRB) | 0.8794 | 0.8705 | -6.33 pts | 8548 ns/victim | 3.42 ms | 5.76 ms | 51,211 req/s |

The learned policy wins on hit ratio by 0.62 points and closes about 9% of the gap that remained to optimal.
It costs roughly 10x more per eviction.

### The arithmetic that makes the win small

At 0.118 evictions per request, an extra 7.7 µs per eviction is about **0.9 µs per request spent**.
0.62 points of hit ratio against a 200 µs origin is about **1.2 µs per request saved**.

Close to break-even, which the throughput figures agree with: 51,967 req/s against 51,211, a 1.5% loss.

This is the honest headline.
A learned policy pays off in proportion to what a miss costs, and this workload has a very fast local origin, which is close to the worst case for it.
Against a 20 ms origin the same 0.62 points would save 124 µs per request and the eviction cost would be noise.
Turning that observation into a curve, hit ratio and throughput against origin latency from tens of microseconds to tens of milliseconds, is the most useful measurement this project does not yet have.

### The gap between the microbenchmark and the live number

The ROADMAP has carried this as "two orders of magnitude, unexplained".
Part of it is now explained, and the explanation is that the microbenchmark was not measuring the same model.

`BenchmarkEvict/lrb` installs a synthetic **single-tree, 25-leaf** staircase over one feature.
It exists to prove the learned path allocates nothing and picks the victim the model says, and it is fine for that.
It is not a model-size measurement, and reading 254 ns/victim as a prediction for a 31-tree fit was the mistake.

Corrected arithmetic, all from the table above:

| Component | Estimate |
| --- | --- |
| Sampling 8 candidates, LRU baseline | 195 ns |
| Feature extraction, 8 candidates | ~60 ns, from the 254-195 difference less one tree |
| Model evaluation, 31 trees, 8 candidates | ~908 ns |
| **Predicted** | **~1.2 µs/victim** |
| **Measured live** | **8.5 µs/victim** |

So the real discrepancy is about 7x, not 30x.
What remains is still unexplained rather than explained away.
The plausible causes are that the microbenchmark holds a hot model and a working set that both fit in cache while the live node has a 6 MiB working set of pointer-chased map entries, and that `evict_ns_mean` under contention includes time the goroutine spent descheduled.
Neither has been demonstrated.

**This needs a CPU profile of a cache node under load before anything here claims to explain the number.**
`/debug/pprof` is on every service's debug port, so the data is one command away; the work is doing it and reading it.

The containerised CI run measured 11,781 ns/victim on GitHub's shared runners, which is consistent with the live figure plus virtualisation overhead and is not an independent data point.

## Reproducing any of this

```sh
make bench-micro                      # the microbenchmark table
make up && make bench                 # the cluster numbers, LRB with no model yet
make train && make bench              # again, with a model installed
CACHE_POLICY=lru make up && make bench  # the baseline
```

`loadgen` prints the offline Belady MIN for the same trace it just replayed, so the gap column comes out of the same run rather than being computed separately.
Set `MIN_OBJECT_HIT` and `MAX_P99` and it exits non-zero when a run misses either, which is how the integration workflow turns a policy or hot-path regression into a red build rather than a number nobody read.

Latencies are exact percentiles off a fully retained sorted sample, not histogram buckets.
That costs 8 bytes per request in `loadgen` and removes any question about bucket boundaries at p99.9.
