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
| `model/RawVectorPool` 13 trees, 512 vectors | 62.05 ns/op, 496.4 ns/eviction, 0.955 ns/nodevisit | 0 |
| `model/RawVectorPool` 13 trees, 2048 vectors | 151.9 ns/op, 1215 ns/eviction, 2.34 ns/nodevisit | 0 |
| `model/RawVectorPool` 13 trees, 8192 vectors | 215.0 ns/op, 1720 ns/eviction, 3.31 ns/nodevisit | 0 |
| `model/RawVectorPool` 13 trees, 262144 vectors | 236.6 ns/op, 1893 ns/eviction, 3.64 ns/nodevisit | 0 |
| `model/RawVectorPool/rowsonly` 13 trees, any pool size | 43.1 to 44.1 ns/op, flat | 0 |
| `model/Load` 100 trees | 1.26 ms/op, 263 MB/s | 2346 |
| `belady/MINHitRatio` 200k requests, 100k keys, 10k capacity | 13.8 ms/op | 208k |

The `model/RawVectorPool` rows are medians of five runs at `-benchtime=2s` rather than the single `make bench-micro` pass, because the effect they measure spans 3.8x and the run-to-run spread on a laptop is a few percent.
The command is in [Reading the model numbers](#reading-the-model-numbers) below.

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

Every `model/Raw` row evaluates **one** feature vector in a loop, so read all of them as a floor.
The rest of this section is about how far below the real cost that floor sits, and the answer turned out to be up to 3.8x.

`RawVaryingFeatures` is the same measurement with a different feature vector per call.
A tree walk is a chain of data-dependent branches, so evaluating one hot vector in a loop lets the branch predictor learn the entire walk, and nothing live is predictable that way.
The 1.35x it reports **is not the size of that effect**, and this document said it was until `RawVectorPool` was written to check.

`RawVaryingFeatures` cycles 512 distinct vectors, and 512 vectors through 13 trees is 6656 root-to-leaf paths, which is a set small enough to memorise.
Growing only the pool, with the model and the loop unchanged, takes 13 trees from 0.955 to 3.64 ns per node visit:

```sh
go test ./internal/model/ -run '^$' -bench RawVectorPool -benchtime=2s -count=5   # medians in the table
```

So the whole range spanned by branch predictability is **3.8x**, not 1.35x, and 60.07 ns/op is barely above the bottom of it.

**It is not the rows going cold**, which is the only other explanation available and would make the finding an artefact of the benchmark's own footprint.
The `rowsonly` control walks the identical pool at the identical stride and touches one float from each row, so it pays the same line fetch, while evaluating a fixed vector.
It is **flat at about 43.6 ns/op from 32 KiB of rows to 16 MiB**, against 62.05 to 236.6 for the real loop.
Sixteen `float32` is one 64-byte line and the walk is sequential, so hardware prefetch covers it and there is nothing left for memory to explain.
`darwin/arm64` exposes no branch-mispredict counter, so naming the mechanism is still inference; ruling out the alternative is not.

### What the model actually costs, which is a range

Neither end of that sweep is the live cost, and the reason is worth stating rather than picking one.

What the predictor memorises is the **path**, not the vector.
Two evictions that sample the same entry a millisecond apart take the same path through most trees, because `recency_ms` moving by one millisecond crosses almost no thresholds.
A shard at live shape holds a few hundred to a few thousand entries and eviction samples eight of them, so the live path set is bounded by the shard population rather than being unbounded.
That puts the live cost between the two ends rather than at either:

| For 13 trees at `num_leaves=32`, 8 candidates | Model cost per eviction |
| --- | --- |
| Lower bound, a fully memorisable path set | 0.50 µs |
| Upper bound, no path ever repeated | 1.89 µs |

Both figures come from `synth`, which builds **complete** trees, so 32 leaves means exactly 5 node visits per tree.
LightGBM grows leaf-wise, so a real `num_leaves=32` tree is unbalanced and its mean path can be longer than 5.
The upper bound is therefore an upper bound on this shape, not on the fit.

The honest reading is that **the sub-microsecond eviction budget is not established by these microbenchmarks in either direction.**
It holds at the memorisable end and fails by about 2x at the other, and where a given workload sits depends on how much path repetition its shard population produces, which nothing here measures.
The earlier claim that the budget held "with roughly half the budget left" came from reading the lower bound as the honest figure.

Everything else that followed from that claim moves with it.
25 trees was described as roughly the full microsecond for eight candidates; at the upper bound the ceiling is nearer **7 trees**, and the 41-tree fits the trainer has produced on this workload are then over budget by 6x rather than by 2x.
That sharpens an existing constraint rather than creating one: `MAX_ROUNDS` is 300 with early stopping at 25, and the only reason a fitted model is ever small is that early stopping fires.
A workload needing 200 trees needs a smaller `CACHE_SAMPLE_SIZE` or a different story about the budget, and on this evidence so does one needing 41.

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

**The second line of that arithmetic is wrong, and the sweep below is what found it.**
Pricing an avoided miss at `ORIGIN_LATENCY` assumes a miss costs what the origin sleeps for.
It does not.
A miss also pays gateway to node to origin gRPC transport, a single-flight rendezvous, and queueing behind 63 other in-flight requests.
Measured on this stack, a miss costs **927 µs at the margin against a flat 200 µs origin**, which is 4.6x the figure used above.
So the saving was understated by 4.6x, the spent side was right, and the direction is no longer in doubt at this operating point.

The correction does not rescue the "0.03% of client latency" observation, which stands.
It changes what that observation means: the win is real and it is invisible, and those are two different claims.

### The origin-latency curve

Everything the learned policy is worth depends on what a miss costs, and the pass above measured exactly one miss cost.
`make bench-sweep` sweeps `ORIGIN_LATENCY` across five points spanning 0 to 20 ms, three repeats per policy per point, 30 runs of 400,000 requests.
Run on 2026-09-16 with `scripts/origin-latency-sweep.sh`, which pins the same configuration as the reproduce block below and prints it before the first run.

**Setup delta from the pass above.**
Only the origin latency moves.
Object sizes come from `Hash(key + "#size")` in `cmd/origin/main.go`, so every point serves byte-identical objects, and `loadgen` reported the same offline Belady MIN of **0.9348 at all 30 runs**, `bytes_used` inside 18.27 to 18.59 MB and zero rejections throughout.
One model spans the whole sweep, because two fits of this workload give different tree counts and a sweep that retrained would put a different model on each half of one curve.
That model is `20260916T212632Z`: **7 trees**, 14 features, holdout AUC 0.9932, 1 s boundary.
Seven is the same AUC as the 13-tree model in the table above from half the inference budget, and it is exactly the ceiling the [tree-count range](#what-the-model-actually-costs-which-is-a-range) puts on a microsecond of eviction.

#### What a miss actually costs

`loadgen` now reports its mean client latency split by whether the gateway answered from cache, so the marginal cost of a miss is measured per run rather than assumed.

| `ORIGIN_LATENCY` | Marginal miss cost | Excess over the configured latency |
| --- | --- | --- |
| 0 | 397 µs | 397 µs |
| 50 µs | 573 µs | 523 µs |
| 200 µs | 927 µs | 727 µs |
| 2 ms | 2765 µs | 765 µs |
| 20 ms | 21,006 µs | 1006 µs |

Median of three repeats, averaged across the two policies, which measure the same origin and agreed to within 6% at every point.

Above 200 µs this is affine with **slope 1.014 and intercept 730 µs**, and the local slopes are 1.021 from 200 µs to 2 ms and 1.013 from 2 ms to 20 ms.
So the origin honours the latency it is configured with, and a miss on this stack costs that latency plus about 0.73 ms of transport, rendezvous and queueing.

Below 200 µs the local slope is 2.4 to 3.5, which is not the origin misbehaving.
It is the queue draining: a cheaper miss means fewer misses outstanding at once, so each one waits behind less work, and the floor falls from 730 µs to 397 µs as the configured latency goes to zero.
That is the closed loop showing up in the one quantity this sweep can measure cleanly, and it is the same coupling the marker on `results.mean` in `cmd/loadgen/main.go` names.

That 397 µs floor is a property of `CONCURRENCY=64`, not of the cache.
Little's Law holds on this data to within 1% at four of the five points and 4% at 20 ms, which is what a closed loop with 64 workers is supposed to do, and the mean latency of a *hit* at a zero-latency origin is 2100 µs, three orders of magnitude above the work a hit performs.
So the absolute latencies here are transport and queueing almost end to end, and the split between the two inside that 397 µs is not measured: 64 was chosen to keep three nodes busy for a hit-ratio comparison, and it silently sets the queueing term in every latency number that followed.
A concurrency sweep at one fixed origin latency would separate them, and until that exists the break-even below should be read as "this deployment at this concurrency", not as a property of the stack.

#### What the policy costs

| `ORIGIN_LATENCY` | LRU evict | LRB evict | Evictions/request | Spent per request |
| --- | --- | --- | --- | --- |
| 0 | 1491 ns | 5553 ns | 0.1128 | 458 ns |
| 50 µs | 1582 ns | 5909 ns | 0.1123 | 486 ns |
| 200 µs | 1844 ns | 6975 ns | 0.1211 | 621 ns |
| 2 ms | 1835 ns | 6367 ns | 0.1117 | 506 ns |
| 20 ms | 2299 ns | 7599 ns | 0.1092 | 579 ns |

This is the constancy the curve needs, and it holds: **530 ns per request, spread 458 to 621**, or ±15% with no trend against a 400x change in origin latency.
The learned policy's price does not depend on what a miss costs, which is what the model not being able to see the origin predicts.

#### What the policy buys, and why this half is not measurable here

| `ORIGIN_LATENCY` | LRU hit | LRB hit | Δhit | LRB spread over 3 repeats | LRU evictions/request |
| --- | --- | --- | --- | --- | --- |
| 0 | 0.8598 | 0.8678 | +80 bp | 3 bp | 0.1213 |
| 50 µs | 0.8583 | 0.8680 | +97 bp | 6 bp | 0.1228 |
| 200 µs | 0.8586 | 0.8596 | +10 bp | 118 bp | 0.1221 |
| 2 ms | 0.8667 | 0.8682 | +15 bp | 74 bp | 0.1142 |
| 20 ms | 0.8739 | 0.8689 | **-50 bp** | 177 bp | 0.1058 |

**Δhit is not constant, and the reason is the load generator rather than the policy.**
This was the one hypothesis the sweep was built to check, and it fails.

Read the last column.
LRU's evictions per request falls monotonically from 0.1213 to 0.1058, a 13% drop, while the offline Belady MIN for the replayed trace is fixed at 0.9348 at every point.
The trace did not change, so the *stream the cache saw* did.

The mechanism is that `loadgen` is a closed loop of 64 workers.
A worker that misses stalls for the full miss cost while the other 63 keep going, so a slow origin rate-limits cold-key traffic specifically and lets hot-key traffic stream through untouched.
The access stream reaching the cache is therefore enriched in hot keys in proportion to origin latency, and every policy's hit ratio rises for free.
It rises *more* for the policy with more misses to be throttled, which is LRU: it gains 141 bp from 0 to 20 ms while LRB's median moves 11 bp.
That compresses Δhit toward zero and then past it.

The same mechanism explains the spread column.
At 0 and 50 µs the origin barely perturbs the generated order and LRB's hit ratio repeats to 3 and 6 basis points across three cold runs.
From 200 µs up, worker interleaving becomes latency-dependent and the spread grows to 177 bp, which is 18x the effect being measured.

So **`Δhit` is only trustworthy at the two points where the origin is too fast to reorder the workload**, and the honest reading of the +80 and +97 bp there is that it agrees with the +66 bp of the pass above to within the difference a 7-tree model makes.
The three high-latency points measure a confounded quantity and are reported here because the confound is the finding.

#### The crossing

```
net advantage per request = Δhit x marginal_miss_cost - Δevict x evictions_per_request
```

Setting that to zero at the two clean points gives the marginal miss cost at which this model breaks even:

| `ORIGIN_LATENCY` | Spent | Δhit | Break-even marginal miss cost |
| --- | --- | --- | --- |
| 0 | 458 ns/request | 0.0080 | **57 µs** |
| 50 µs | 486 ns/request | 0.0097 | **50 µs** |

Call it **50 to 57 µs**, or 45 to 70 µs allowing 6 bp of hit-ratio noise and the ±15% on spent.

**The crossing lies below this stack's floor, so there is no origin latency at which the learned policy loses here.**
The cheapest miss the sweep could construct, against an origin that sleeps for zero, still costs 397 µs at the margin, which is 7x the break-even.
At the 200 µs anchor the policy spends 621 ns per request and saves 0.0080 x 927 µs = 7.4 µs per request, and at 20 ms it would save 168 µs per request against the same 579 ns.

That inverts the recorded headline, and it does so by correcting an arithmetic error rather than by measuring anything the pass above could not have measured.
"At what origin latency does a learned policy start to pay" turns out to be the wrong question for this deployment, because the answer is below zero.
The right form of the answer is the one in marginal miss cost: **it pays when a miss costs more than about 55 µs at the margin**, and a miss on any stack with a network between the cache and its backing store costs far more than that.
A deployment whose transport floor were under 55 µs, meaning an in-process origin with no gRPC and no queue, would sit on the other side, and that is the case this sweep cannot reach.

**This crossing is arithmetic on stable measurements, not a sign flip anyone watched happen.**
Client latency cannot resolve it: the predicted difference is 0.03% of the mean at 200 µs and about 4% at 20 ms, against a mean that carries the 17% throughput spread above, because mean latency in a closed loop is roughly `concurrency / throughput`.
The measured means bear that out and settle nothing, at every point.

#### The tail, separately

The curve is a mean model, and p99 is not the mean.

| `ORIGIN_LATENCY` | LRU p99 | LRB p99 | LRU p99.9 | LRB p99.9 |
| --- | --- | --- | --- | --- |
| 0 | 5.54 ms | 6.17 ms | 10.18 ms | 13.68 ms |
| 50 µs | 5.34 ms | 5.83 ms | 7.97 ms | 11.11 ms |
| 200 µs | 6.53 ms | 7.24 ms | 11.62 ms | 13.57 ms |
| 2 ms | 7.04 ms | 7.12 ms | 10.47 ms | 10.08 ms |
| 20 ms | 24.37 ms | 26.28 ms | 33.71 ms | 48.78 ms |

The medians favour LRU at p99 at all five points, by 1% to 11%.
Run by run LRU wins 10 of 15 paired repeats at p99 and 11 of 15 at p99.9, so this is a lean and not a result.
What it does do is fail to reproduce the pass above, where LRB was clearly ahead at p99 (5.20 ms against 6.29 ms) and behind at p99.9.
Two passes disagreeing on the sign of a tail difference this small is the tail difference being noise, and the p99 line in that table should be read as unmeasured rather than as a win.

#### What this does not deliver

The roadmap asked for hit ratio *and throughput* against origin latency.
Throughput is still the 17% spread, and the sweep makes that worse rather than better: `req_per_sec` falls from ~29,000 to ~14,000 across the range purely because each worker spends longer blocked, which is arithmetic, not a policy comparison.

Both open halves want the same fix, which is why it is now one item rather than two.
An **open-loop generator driving a fixed arrival rate** would stop a slow origin from reordering the workload, which is what contaminated Δhit, and would stop throughput from being a restatement of mean latency, which is what makes it unmeasurable.
Until that exists, the two clean points are the whole hit-ratio result and the marginal miss cost is the whole latency result.

### The gap between the microbenchmark and the live number

This started as "two orders of magnitude, unexplained", became 7x after the microbenchmark's model size was corrected, then closed to about 1.4x, and the live figure now falls **inside** a predicted range rather than beside a predicted point.

The reason it stayed open so long is worth more than the number.
Every attempt to explain it looked at the learned policy's eviction cost and asked what the model was doing.
The answer was that most of the gap was never about the model: **LRU's eviction cost inflates by the same factor between the two environments**, and nobody had compared the baselines.

Four factors, each measured rather than argued:

| Factor | Evidence | Size |
| --- | --- | --- |
| Container, three nodes, eight shared cores | `EvictLiveShape/lru` 296.6 ns/victim native, LRU 1867 ns/victim live, identical work | 6.3x |
| Working set and concurrency | `Evict/lru` 191.3 ns/victim, `EvictLiveShape/lru` 296.6 ns/victim | 1.55x |
| Branch predictability in the tree walk | `RawVectorPool` 0.955 ns/nodevisit at 512 vectors, 3.64 at 262144, `rowsonly` flat | 3.8x |
| Windowing the counter instead of reading a lifetime mean | `Evict/lru` 195 → 191.3 ns/victim | 1.02x |

The third row was recorded as 1.35x from `RawVaryingFeatures` and is the one number a later pass changed by a large factor.
It is also the only one of the four that is a **range** rather than a point, which is why the prediction below became a range too.

Rebuilt prediction for the model this pass actually trained, all from the microbenchmark table:

| Component | Estimate |
| --- | --- |
| Sampling 8 candidates at live shape, LRU baseline | 297 ns |
| Feature extraction plus one tree, 8 candidates | 191 ns, from the `EvictLiveShape` lrb-lru difference |
| Model evaluation, remaining 12 of 13 trees, memorisable path set | 458 ns |
| Model evaluation, remaining 12 of 13 trees, no path repeated | 1747 ns |
| **Predicted, native** | **946 to 2235 ns/victim** |
| **Predicted, live**, applying the 6.3x environment factor | **6.0 to 14.1 µs/victim** |
| **Measured live** | **8.3 µs/victim** |

The measurement lands inside that bracket, about a fifth of the way up it, so there is no residual left to attribute.
That is a weaker claim than the 1.4x it replaces, and a truer one: the previous pass had a point prediction only because it read the memorisable end of the pool sweep as the model's cost, and a residual it then explained by arguing that the LRU-derived 6.3x should not apply cleanly to branch-bound work.

That argument may still be right, and it is no longer needed.
Where the live figure sits inside the bracket is itself the more informative result: near the bottom, which is what the shard-population argument predicts.
Eviction samples eight entries from a shard holding a few hundred to a few thousand, and a sampled entry's path through the trees barely moves between two evictions milliseconds apart, so the live path set is closer to memorisable than to fresh.

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

### Batching the eight candidates into one pass, which does not work

That 74% names the tree walk as the only target worth touching, and the obvious change is to score all eight sampled candidates in one pass over the trees instead of eight separate `Raw` calls.
The candidates are entirely independent, and each walk is a chain of two dependent loads per level: the node, then the feature the node names, whose address depends on the first load.
Interleaving should hand the load unit several chains at once.

Four prototypes were written in the test file and measured before anything touched `internal/cache`: tree-outer with candidate-inner, level-lockstep over a fixed `[8]int32`, and hand-unrolled scalar locals at widths 4 and 8.
They are in commit `a867f09` and were then removed, because at the live shape of 13 trees and a batch of 8 the best of them was **1.04x to 1.09x**, against a stopping rule that wanted 1.20x.

Sweeping tree count looked briefly like a reason to ship anyway, and then failed to reproduce.
Three runs of the same command gave the best variant at 25 trees as 2.13x, 1.90x and 1.15x, and one run had `seq` winning at 41, 50 and 100 trees while another had it losing at all three.
That sweep rebuilds a model per shape, so a 100-tree model's node array is about 100 KiB and leaves L1 while a 13-tree model's 6.4 KiB does not, but a spread that wide between runs of one command is machine noise and no amount of interpretation fixes it.
**Nothing in this section rests on it.**

Holding the model at 13 trees and growing only the vector pool does reproduce, across runs and in sign:

| Vector pool | 8 sequential `Raw` calls | Best batched variant | Ratio |
| --- | --- | --- | --- |
| 512 | 501.1 ns | 481.5 ns | 1.041x |
| 1024 | 613.8 ns | 546.4 ns | 1.123x |
| 2048 | 1289 ns | 1202 ns | 1.072x |
| 4096 | 1644 ns | 1659 ns | 0.991x |
| 8192 | 1743 ns | 2046 ns | 0.852x |
| 32768 | 1979 ns | 2173 ns | 0.911x |
| 262144 | 1931 ns | 2202 ns | 0.877x |

```sh
git stash && git checkout a867f09 -- internal/model/rawbatch_test.go
go test ./internal/model/ -run '^$' -bench BenchmarkRawBatchVectorPool -count=7   # medians above
```

**Once the branches are genuinely unpredictable, batching is 9 to 15% slower.**
Every apparent win was measured in the regime where the predictor was doing the work, and the live cache is not in that regime.

Two things that were true before the measurement and stayed true: `ROADMAP.md` used to attribute the opportunity to the 1.35x hot-versus-varying gap, which was never the mechanism, because batching does not make a feature vector fixed.
And the arithmetic said the win would be small before any of this ran: 60.07 ns/op over 65 node visits is about 3 cycles per visit on an M4, which a strictly serial two-load chain cannot reach, so `Raw` was already overlapping roughly three chains and batching could only deepen parallelism that was partly present.

`internal/cache/lrb.go` and `internal/model/model.go` are therefore unchanged.
What the pass produced instead is the pool sweep above, which is worth more than a win of a few percent in a component that costs 0.7 µs per request against a 1.84 ms p50.

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

Three fits of this same workload at the same boundary have now come out at **13 trees** on 80,120 rows at 0.9932 AUC, **41 trees** on 80,182 rows at 0.9948, and **7 trees** at 0.9932.
The row counts agree to 0.08% and the tree count varies by 6x, which is early stopping responding to which keys the hash happened to sample.
That matters because trees are the inference budget: the 41-tree model costs roughly six times as much per eviction as the 7-tree one for 16 basis points of AUC.
So a tree count is a property of one training run, and the LRB eviction cost moves with it: 8343 ns/victim at 13 trees in the table above, 6975 ns/victim at 7 trees at the same origin latency in the sweep.
Expect the shape of the result to repeat and the third decimal place not to, and read `trees` out of the trainer's output on every fit rather than assuming the last one holds.

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

# The origin-latency curve, about 35 minutes. Continues from the state above rather
# than starting over: it needs exactly one model in the volume, so do not down -v and
# do not train again between here and there.
make bench-sweep                                # 30 runs -> sweep.jsonl, then the table
./scripts/origin-latency-sweep.sh --summarize sweep.jsonl   # re-read it for free
```

The sweep owns the pinned block above rather than inheriting it, so the exports are not required for `make bench-sweep`, only for the single runs.
It aborts rather than recording a row when a cluster reports `policy: "mixed"` or when an `lrb` run has an empty `model_version`, because both look like a measurement and are not.

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
