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
Apple M4, `darwin/arm64`, 2026-09-14, with no containers running.

| Benchmark | Result | Allocations |
| --- | --- | --- |
| `cache/GetHit` (parallel, 256 shards) | 21.53 ns/op | 0 |
| `trace/RingPush` | 13.67 ns/op | 0 |
| `cache/Evict/s3fifo` | 79.57 ns/victim | 0 in the policy |
| `cache/Evict/lfu` | 181.0 ns/victim | 0 in the policy |
| `cache/Evict/lru` | 191.3 ns/victim | 0 in the policy |
| `cache/Evict/lrb` | 256.0 ns/victim | 0 in the policy |
| `cache/EvictLiveShape/s3fifo` | 236.4 ns/victim | 0 in the policy |
| `cache/EvictLiveShape/lru` | 296.6 ns/victim | 0 in the policy |
| `cache/EvictLiveShape/lfu` | 307.2 ns/victim | 0 in the policy |
| `cache/EvictLiveShape/lrb` | 487.1 ns/victim | 0 in the policy |
| `cache/LRBVictim` | 176.6 ns/op | 0 |
| `hashring/Pick` (parallel, 8 nodes, 256 replicas) | 341.1 ns/op | 0 |
| `model/Raw` 50 trees, 32 leaves | 192.3 ns/op, 1538 ns/eviction | 0 |
| `model/Raw` 100 trees, 64 leaves | 466.2 ns/op, 3730 ns/eviction | 0 |
| `model/Raw` 200 trees, 64 leaves | 1038 ns/op, 8305 ns/eviction | 0 |
| `model/RawVaryingFeatures` 13 trees, one hot vector | 44.61 ns/op, 356.8 ns/eviction | 0 |
| `model/RawVaryingFeatures` 13 trees, 512 vectors | 60.07 ns/op, 480.6 ns/eviction | 0 |
| `model/Load` 100 trees | 1.26 ms/op, 263 MB/s | 2346 |
| `belady/MINHitRatio` 200k requests, 100k keys, 10k capacity | 13.8 ms/op | 208k |

The 3 allocations per op reported by `Evict/*` are the benchmark's own `fmt.Sprintf` and the value slice, not the policy.
`model/Load` allocates freely on purpose: it runs off the request path, once per model version, and 1.3 ms to parse a 100-tree dump is not a number worth optimising.

`Evict/*` reports the store's own eviction timer, which measures only `policy.Victim` plus one clock read.
It is the same counter the running cluster exports, and both now report it the same way: as a mean over the measured window rather than over the process lifetime.
That last part had to be fixed before the two numbers could be compared at all, and it is the smaller of the two reasons they used to disagree.

### Two shapes, because one of them was hiding a factor of 1.55

`Evict/*` uses one shard over 512 KiB from a single goroutine.
That fits in L2, cannot contend, and is the right shape for the question it was written for: does the eviction path allocate, and does each policy pick the victim it claims to.

`EvictLiveShape/*` is the same loop at the shape a cache node actually runs: 6 MiB over 32 shards, 1456-byte objects as measured live, driven by `RunParallel`.
LRU costs 191.3 ns/victim at the first shape and 296.6 ns/victim at the second, for identical work.
That 1.55x is working set and concurrency, and it is charged to every policy, which is exactly why looking only at the learned policy's number could not find it.

### Reading the model numbers

The `ns/eviction` column on `model/Raw` is the per-evaluation cost times 8, the default sample size.
A 50-tree, 32-leaf model therefore costs about 1.5 µs per eviction in evaluation alone, which is over the budget.

`RawVaryingFeatures` is the same measurement with a different feature vector per call, and it is the honest one.
A tree walk is a chain of data-dependent branches, so evaluating one hot vector in a loop lets the branch predictor learn the entire walk.
Nothing live is predictable that way: eight candidates per eviction, each with its own recency, size and access history.
The difference is 1.35x, and every `model/Raw` row above understates its model by about that much.

The fit measured in the pass below is 13 trees at `num_leaves=32`, which costs **about 0.5 µs per eviction** in evaluation with varying features.
So the sub-microsecond budget holds for the model that gets trained, with roughly half the budget left, and it is exceeded somewhere above 25 trees.
That is a real constraint on the trainer, not a theoretical one: `MAX_ROUNDS` is 300 with early stopping at 25, and the only reason the fitted model is small is that early stopping fires.
A workload that needed 200 trees would need either a smaller `CACHE_SAMPLE_SIZE` or a different story about the budget.

## Measured cluster numbers

The step 16 confirmation pass, run from a torn-down state on 2026-09-14.
It replaces an earlier pass whose configuration was not fully recorded; see [what the earlier pass could not have been](#what-the-earlier-pass-could-not-have-been).

**Setup.**
Three cache nodes in Docker, 32 shards each, 6 MiB per node.
Zipfian `s = 1.1` over 50,000 keys, 400,000 requests at concurrency 64, 20,000-request warmup excluded from the numbers.
Pareto object sizes, `alpha = 1.5` from a 512-byte minimum, which came out at a **1456-byte observed mean**.
Origin latency a flat 200 µs with jitter off, so the break-even arithmetic below uses an exact miss cost.
Apple M4, 8 cores, everything containerised including the gateway and origin, `loadgen` native on the host.
Each policy starts cold, from `--force-recreate` on the three nodes.
The model is a 13-tree LightGBM fit on 80,120 rows sampled from a 13.9 s trace of the same workload, holdout AUC 0.9932, positive rate 0.0277, at a **1 s boundary**.

| Policy | Object hit | Byte hit | Gap to Belady MIN | Evict cost | p99 | p99.9 |
| --- | --- | --- | --- | --- | --- | --- |
| LRU (sampled, 8) | 0.8636 | 0.8545 | -7.12 pts | 1867 ns/victim | 6.29 ms | 11.88 ms |
| Learned (LRB, 13 trees) | 0.8702 | 0.8608 | -6.45 pts | 8343 ns/victim | 5.20 ms | 12.90 ms |
| LRB with no model loaded | 0.8632 | 0.8546 | -7.16 pts | 1803 ns/victim | 6.03 ms | 16.29 ms |

The learned policy wins on hit ratio by 0.66 points and closes about 9.4% of the gap that remained to optimal.
It costs 4.5x more per eviction.

The third row is the control: `lrb` configured with no model falls back to recency, and it lands on LRU's numbers to within 4 basis points.
A run labelled "lrb" that quietly never loaded a model would look exactly like that, which is why the row is here.

**Throughput is deliberately absent from that table.**
Four runs of the same configuration on this machine reported 26,511, 28,670, 30,343 and 31,110 req/s, a 17% spread, because three cache nodes plus a gateway, an origin and `loadgen` are competing for eight cores.
Hit ratio over the same four runs varied by 4 basis points and eviction cost by 4%, so those two are measured and throughput is not.
Deciding the throughput comparison needs a quieter machine or a longer run, and it is the one number this pass does not settle.

### The arithmetic that makes the win small

At 0.111 evictions per request, an extra 6.5 µs per eviction is about **0.72 µs per request spent**.
0.66 points of hit ratio against a flat 200 µs origin is **1.32 µs per request saved**.

So the learned policy is about 0.6 µs per request ahead here, against a p50 of 1.84 ms.
That is 0.03% of the latency a client sees, and far inside the run-to-run spread above.
Call it break-even and do not claim a direction.

This is the honest headline.
A learned policy pays off in proportion to what a miss costs, and this workload has a very fast local origin, which is close to the worst case for it.
Against a 20 ms origin the same 0.66 points would save 132 µs per request and the eviction cost would be noise.
Turning that observation into a curve, hit ratio and throughput against origin latency from tens of microseconds to tens of milliseconds, is the most useful measurement this project does not yet have.

### The gap between the microbenchmark and the live number

This started as "two orders of magnitude, unexplained", became 7x after the microbenchmark's model size was corrected, and is now accounted for to within about 1.4x.

The reason it stayed open so long is worth more than the number.
Every attempt to explain it looked at the learned policy's eviction cost and asked what the model was doing.
The answer was that most of the gap was never about the model: **LRU's eviction cost inflates by the same factor between the two environments**, and nobody had compared the baselines.

Four factors, each measured rather than argued:

| Factor | Evidence | Size |
| --- | --- | --- |
| Container, three nodes, eight shared cores | `EvictLiveShape/lru` 296.6 ns/victim native, LRU 1867 ns/victim live, identical work | 6.3x |
| Working set and concurrency | `Evict/lru` 191.3 ns/victim, `EvictLiveShape/lru` 296.6 ns/victim | 1.55x |
| Branch predictability in the tree walk | `RawVaryingFeatures` hot 44.61 ns/op, varying 60.07 ns/op | 1.35x |
| Windowing the counter instead of reading a lifetime mean | `Evict/lru` 195 → 191.3 ns/victim | 1.02x |

Rebuilt prediction for the model this pass actually trained, all from the microbenchmark table:

| Component | Estimate |
| --- | --- |
| Sampling 8 candidates at live shape, LRU baseline | 297 ns |
| Feature extraction plus one tree, 8 candidates | 191 ns, from the `EvictLiveShape` lrb-lru difference |
| Model evaluation, remaining 12 of 13 trees, varying features | 444 ns |
| **Predicted, native** | **~930 ns/victim** |
| **Predicted, live**, applying the 6.3x environment factor | **~5.9 µs/victim** |
| **Measured live** | **8.3 µs/victim** |

A 1.4x residual, against 7x before.
It is not decomposed further, and the honest reading is that the 6.3x environment factor was derived from LRU, whose cost is dominated by memory latency, while model evaluation is compute and branch bound.
There is no reason those two should scale identically under CPU oversubscription, and 1.4x is about the size of that mismatch.

### What was ruled out

`evict_ns_mean` brackets `policy.Victim` while the shard mutex is already held, so time the goroutine spent descheduled would be charged to the policy.
That was the leading hypothesis and it is **wrong**.

`PROFILE_CONTENTION=true` arms the runtime's mutex and block samplers, and over a full 400,000-request run against the learned policy:

- The mutex profile attributes **4.13 ms** to the shard lock, all of it on `shard.get`, the read path. `shard.insert` and `evictOne` do not appear at all.
- The block profile attributes **nothing** to the `cache` package. It is entirely `selectgo` and `chanrecv` in the trace recorder and in gRPC, which is goroutines waiting for work rather than contending.
- A 20-second CPU profile puts `lrb.Victim` at 0.19 s cumulative on-CPU, which matches the wall time the counter reports for the same interval to within sampling error. If descheduling were the story, wall time would exceed on-CPU time.

The same CPU profile splits `Victim` cleanly, and the split is what the rebuilt prediction above is checked against:

| Inside `lrb.Victim` | Share of on-CPU time |
| --- | --- |
| `model.Raw`, the tree walk | 74% |
| `shard.Sample`, the map range and pointer chase | 10% |
| `features.Extract` and `Entry.Deltas` | 16% |

Sampling is 10% of the learned policy's eviction cost.
The `Entry` layout and the map-range sampling in `shard.Sample` were the obvious things to optimise and they are not where the time is.

The integration workflow reports **28,968 ns/victim** on GitHub's shared runners against the 8,343 measured here.
It is not an independent data point, but the direction of both differences is informative: it fit 41 trees rather than 13, which is roughly 3x the tree walk, and it runs the whole cluster on two shared vCPUs, which is more of the same oversubscription that the 6.3x environment factor above measures.

That figure was 11,781 before this pass, and the change is the counter rather than the machine.
`evict_ns_mean` was a lifetime mean, so the model-loaded replay's evictions were averaged together with the cheap fallback evictions of the replay that captured the trace.
Windowing it over the measured interval raised the number by 2.5x, which is the clearest single argument for the change: the old figure was not a smaller measurement of the same thing, it was a measurement of a different thing.

### What the earlier pass could not have been

The pass this section replaces recorded "Apple M4, native, not in Docker" and a 31-tree model at holdout AUC 0.985, fit on 217,283 rows from an 8.5 s trace.
It did not record `MODEL_BOUNDARY`, and it cannot have been the documented default of `10m`.

Nothing in an 8.5-second trace is reused more than 600 seconds later, so at a 600 s boundary every row is labelled "within boundary" and the trainer refuses to fit:

```
error: 0.0000% of rows are labelled 'beyond boundary', which is too one-sided to
rank candidates with. The 600s boundary sits outside the reuse times in this trace
```

That check has been in `trainer/belady_trainer/train.py` since it was written, before the earlier pass was recorded, so the earlier pass was run at some other boundary that was never written down.
The pass above uses `MODEL_BOUNDARY=1s`, which the reproduce block pins, and 1 s is close to forced: the reuse-time distribution on this workload is p50 0.2 ms, p90 35 ms, p99 2.79 s, so a boundary has to sit in the p90-p99 range to give a usable class balance, and the registry metadata carries whole seconds.
The resulting positive rate is 0.0277 against a 0.02 floor, which is not much margin.

### What is not reproducible even with a fixed seed

`loadgen` takes `SEED` and replays a deterministic key sequence, so hit ratios repeat to a few basis points.
The model does not.

Trace sampling is one key in sixteen **by key hash**, so which keys land in the sample depends on the hash and the sampled fraction swings between runs.
The tree count is an outcome of early stopping, not a setting.

Two fits of this same workload, minutes apart at the same boundary, came out at 13 trees on 80,120 rows at 0.9932 AUC and **41 trees on 80,182 rows at 0.9948**.
The row counts agree to 0.08% and the tree count varies by 3x, which is early stopping responding to which keys the hash happened to sample.
That matters because trees are the inference budget: the second model would cost roughly three times as much per eviction for 16 basis points of AUC.
So the 13-tree model is a property of one training run, and the LRB eviction cost in the table above moves with it.
Expect the shape of the result to repeat and the third decimal place not to.

## Reproducing any of this

```sh
make bench-micro                        # the microbenchmark table

# The cluster numbers. Every value here is load-bearing and none of them are the
# committed defaults, which is why they are spelled out rather than left implicit.
export CACHE_CAPACITY=6MiB CACHE_SHARDS=32 MODEL_BOUNDARY=1s
export ORIGIN_LATENCY=200us ORIGIN_JITTER=0
export REQUESTS=400000 KEYSPACE=50000        # CONCURRENCY, WARMUP, ZIPF_S, SEED are defaults
export ORIGIN_MIN_SIZE=512 ORIGIN_MAX_SIZE=65536 ORIGIN_SIZE_ALPHA=1.5

docker compose -f deploy/compose.yaml down -v   # clean state: the models volume too
CACHE_POLICY=lrb make up && make bench          # LRB with no model yet, and this captures the trace

# make train reads ../traces on the host, which make up does not populate: traces
# live in a Compose volume. Use the containerised trainer, or bring the stack up
# with make up-dev, which bind-mounts them.
make train-compose
docker compose -f deploy/compose.yaml up -d --force-recreate cachenode-0 cachenode-1 cachenode-2
make bench                                      # LRB with the model installed

CACHE_POLICY=lru make up && make bench          # the baseline
```

The recreate step is not optional.
Nodes validate a model's `boundary_seconds` against their own `MODEL_BOUNDARY` and refuse a mismatch, so nodes started before the boundary was chosen will not accept the model:

```
ERROR refused model version=20260914T054948Z
  err="trained against a 1s Belady boundary, this node is configured for 10m0s"
```

`make train-compose` still reports success, because publishing succeeded, and the next `make bench` then measures the fallback policy under the label `lrb`.
The third row of the table above is what that looks like, and the only way to notice it in the moment is the node logs.

For the contention profiles, add `PROFILE_CONTENTION=true` before `make up` and read `/debug/pprof/mutex` and `/debug/pprof/block` off port 9201.
Do not take throughput or latency from a run with it on.

`loadgen` prints the offline Belady MIN for the same trace it just replayed, so the gap column comes out of the same run rather than being computed separately.
Set `MIN_OBJECT_HIT` and `MAX_P99` and it exits non-zero when a run misses either, which is how the integration workflow turns a policy or hot-path regression into a red build rather than a number nobody read.

Latencies are exact percentiles off a fully retained sorted sample, not histogram buckets.
That costs 8 bytes per request in `loadgen` and removes any question about bucket boundaries at p99.9.
