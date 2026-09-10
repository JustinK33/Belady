# Learned eviction

## Belady's MIN

In 1966 László Bélády proved which eviction policy is optimal.
When the cache is full, evict the object whose next access is furthest in the future.
No online policy can do better on hit count, and the proof is a straightforward exchange argument: any policy that evicts something else can be rewritten to evict MIN's choice without losing a hit.

MIN needs the future, so it cannot be implemented.
That is usually where the discussion ends, and every production cache ships a heuristic instead, almost always LRU.

But MIN is still useful offline.
Given a recorded trace you can compute exactly what MIN would have achieved, which turns "our hit ratio is 0.88" into "our hit ratio is 0.88 against a ceiling of 0.94".
That number is the only honest way to say whether a policy is good, as opposed to better than the last one.

`internal/belady/min.go` computes it.
The construction is one backward pass building `next[i]`, the index of the next occurrence of the same object, then a forward simulation with a max-heap keyed on `next`.
Stale heap entries are skipped lazily rather than removed, so the whole thing is O(n log n) with no deletions.

`MINHitRatioFrom(trace, capacity, from)` exists because a warm policy must not be scored against a cold optimum.
`loadgen` replays a warmup and then a measured window; MIN is fed warmup plus the measured window and told to start counting at the boundary, so both sides enter the comparison with a populated cache.
Without that the ceiling can land *below* the policy, which looks like a bug in the policy and is a bug in the measurement.

Two things make this an estimate of the ceiling rather than the ceiling:

- **Capacity is counted in objects, not bytes.**
  `loadgen` converts the byte capacity using the mean object size actually observed.
  With variable object sizes the true offline optimum is a knapsack-over-time problem, NP-hard, normally approximated with min-cost flow.
  The marker is in `internal/belady/min.go`.
- **MIN models one unified cache; the real cluster is nodes times shards, each capped independently.**
  Sharding can only lose hits, so a policy scoring *above* MIN means the object conversion is off, not that the policy beat optimal.

## Relaxed Belady

The obvious way to learn MIN is to predict the time until an object's next access, then evict the largest prediction.
That is a regression problem with a heavy-tailed target, and most of the effort goes into distinguishing outcomes the cache does not care about.
Whether an idle object is next touched in ten minutes or ten hours makes no difference: both are well past the point where holding it pays.

Relaxed Belady, from [Learning Relaxed Belady for Content Distribution Network Caching](https://www.usenix.org/conference/nsdi20/presentation/song) (NSDI 2020), replaces the regression with a threshold.
Pick a **Belady boundary** `B`.
Ask one binary question:

> Is this object's next access more than `B` in the future?

An object answering yes is a good victim.
Among objects answering yes the policy no longer distinguishes, and that is the "relaxation": it gives up the ordering inside the far tail, which is precisely the part of the ordering that does not change the hit ratio.

What is left is binary classification, which a shallow gradient-boosted tree does well, and whose output only needs to be *ordered* correctly across a handful of candidates.
The absolute probability is never used, which is why the metric worth reading is AUC and why `internal/model` evaluates `Raw` rather than `Score`: the logistic is monotone, so it cannot change which candidate ranks worst, and skipping it saves an `Exp` per candidate.

### The boundary is a property of the workload

This is the single easiest thing to get wrong, and it fails silently.

Set `B` far above the reuse times in the trace and every row is labelled "beyond boundary".
Set it far below and none are.
In both cases LightGBM trains happily, reports an excellent accuracy against the majority class, and produces scores with no useful order.
The cache then evicts effectively at random while reporting a model version and a hit ratio.

Two defences:

1. `trainer/belady_trainer/train.py` refuses to fit when the positive rate is outside `MIN_CLASS_RATE`, currently 0.02, and prints the reuse-time quantiles instead of a model.
   `make -C trainer boundary` reports those quantiles on their own.
2. A cache node refuses a model whose `boundary_seconds` disagrees with its own `MODEL_BOUNDARY`.
   A score from a ten-minute model answers "will this go unused for ten minutes"; the same number from a ten-second model answers a different question, and would be ranked as though it answered the first.
   Nothing downstream can detect that, so it is checked against configuration an operator set deliberately.
   The check is in `internal/registry/watcher.go`.

`ModelMeta.boundary_seconds` is whole seconds, which means the smallest legal boundary is 1s.
On a synthetic workload whose median reuse time is milliseconds that floor already sits above the median.
It is workable, and it is listed as an open question in [ROADMAP.md](../ROADMAP.md).

## The feature vector

Fourteen `float32` columns, declared in `internal/features/features.go` and mirrored in `trainer/belady_trainer/columns.py`.

| Index | Name | Meaning |
| --- | --- | --- |
| 0 | `size_bytes` | Object size. |
| 1 | `recency_ms` | Time since the last access. |
| 2 | `age_ms` | Time since admission. |
| 3 | `accesses` | Accesses since admission, saturating at 2^32-1. |
| 4 | `frequency` | Two-bit saturating counter, 0 to 3, also read by S3-FIFO. |
| 5 | `reuse_rate` | `accesses / (age_seconds + 1)`. |
| 6-13 | `delta_0` .. `delta_7` | Gaps between consecutive past accesses in milliseconds, newest first, zero where there is no history. |

Notes that are not obvious from the table:

- **`reuse_rate` is the only column a tree cannot derive from the others.**
  A decision tree thresholds one feature at a time, so it can split on `accesses` and on `age_ms` independently but cannot form their ratio.
  Everything else in the list is either raw state or a monotone function of raw state.
  The `+1` in the denominator stops a freshly admitted object from dividing by nearly zero.
- **Milliseconds, not seconds and not microseconds.**
  A process-local cache sees sub-second reuse constantly, so seconds would quantise most of the signal away.
  A `float32` holds every integer millisecond exactly out to about 4.6 hours; past that the value rounds, which cannot move a threshold split already measured in hours.
- **Eight deltas, not the paper's 32.**
  Eight carries the same shape of signal at a quarter of the per-object metadata cost, and that metadata competes with cached bytes for the same memory budget.
  There is a marker on the constant: widen it if feature importance shows `delta_7` still carrying weight.
- **No value is ever NaN.**
  That is what lets the evaluator be a plain two-way branch per node with no missing-value handling, which is [ADR 0005](adr/0005-hand-rolled-evaluator.md).

The layout is the same contract written twice in two languages, and a mismatch is invisible at runtime: the model loads, evaluates, and reads `size_bytes` wherever it was trained to read `recency_ms`.
`TestNamesMatchTheTrainer` in `internal/features` parses the Python source and fails the Go build if the two ever drift.
It has its own CI job because it is the one failure in this repository that is otherwise completely silent.

## From a trace to labelled rows

The trace a cache node records is deliberately minimal.
One `AccessRecord` is 24 bytes: `key_hash`, `timestamp_us`, `size_bytes`, `hit`.

`key_hash` rather than the key itself, so no plaintext key can reach a training set or a trace file.
`hit` is what makes the rest reconstructable.

### Replaying entry state

The features are derived from state the cache keeps per entry, and none of it is in the trace directly.
`trainer/belady_trainer/samples.py` replays it, and has to match `internal/cache/entry.go` exactly.

The trick is that **a miss is a reset point.**
`hit = false` means the object was not resident, so it was admitted fresh and its counters start from zero.
That is why the trace needs no eviction records at all: every eviction is implied by the next miss on that key.

Given that, replaying is vectorised numpy.
Group by key, chronological within key; a segment starts at the first record of a key and at every miss; position within the segment gives `accesses`, the segment start gives `admitted`, and consecutive differences give the delta history, reset to zero at each segment boundary.

### Choosing the moment to snapshot

This mattered more than any parameter on the booster.

The obvious thing is to emit one training row per access.
Do that and `recency_ms` is identically zero in every row, because no time has passed since the access you are standing on.
At eviction `recency_ms` is whatever it happens to be, it is large, and it is one of the two most decisive columns.
A model trained on rows where it is always zero has never seen the input distribution it runs on.

Instead each inter-access interval contributes one snapshot at a **uniformly random offset** into it.
Recency is then distributed the way the cache actually observes it.

The offset is capped at `MAX_RECENCY_BOUNDARIES` times the boundary, four by default.
Past a few boundaries every row is labelled the same way anyway, so the extra rows carry no signal and only skew recency away from what a resident entry looks like.
The offset range starts at zero, not one, because two accesses to the same key can land in the same microsecond under concurrency, and the only offset into a zero-length interval is zero.

There is a marked simplification here: snapshots are taken without simulating residency, so some rows describe objects a real cache would already have evicted.
That teaches "very idle means evictable", which is correct, and it keeps the training set from depending on the policy that captured the trace.
The trigger for revisiting it is feature importances that start to look policy-shaped.

### The label, and the end of the trace

`y = 1` when the time from the snapshot to the object's next access exceeds the boundary.

The awkward case is an object with no next access in the trace at all.
That is *usually* "never used again", which is a genuine positive, but it is *sometimes* just "the recording stopped".
Those are indistinguishable from inside the file, and if you keep them all the model learns that the end of the recording is a property of the object.

So a censored row survives only if the trace ran at least a boundary past its snapshot.
Everything else is dropped and counted as `dropped_censored`.
On one 8.5 s trace that was 6,856 rows out of 224,139.

### The holdout is split by time

Rows from the same object's history are highly correlated.
A random split puts near-duplicates on both sides and reports an AUC the model cannot reproduce on tomorrow's traffic.
The split is therefore chronological: oldest 80% to train, newest 20% to score.

## Eviction at runtime

`internal/cache/lrb.go`:

1. Load the current model with one atomic pointer read.
   If there is none, delegate to sampled LRU.
2. Draw `CACHE_SAMPLE_SIZE` candidates from the shard's map, eight by default.
3. For each, fill a reused `features.Input` from the entry and extract into a reused `[]float32`.
4. Evaluate `Raw`, keep the running maximum in struct fields.
5. Evict the argmax.

Nothing in that loop allocates.
The candidate visitor is a `func(*Entry) bool` stored on the policy struct at construction, specifically so the closure is not allocated on every eviction under the shard lock.

Sampling instead of a global priority queue is [ADR 0003](adr/0003-sampled-eviction.md).
The short version: a heap over every resident object makes every hit an O(log n) update under a lock, and the policy's whole value is a fractional improvement in victim quality.
Eight random candidates from a shard get most of the way there for a bounded, predictable cost, which is the same trick Redis uses for its sampled LRU.

Sampling relies on Go's randomised map range start, which skews slightly toward fuller hash buckets.
It is marked in `internal/cache/shard.go`.

### The fallback matters

A fresh cluster has no model.
`lrb` with no model installed is sampled LRU wearing a different name, and reporting that as a learned-policy result would be dishonest, so `Stats` carries `model_version` and it is empty until a model is actually installed.
This is also why the default `CACHE_POLICY` is `s3fifo` rather than `lrb`: for someone who is using the cache rather than measuring it, the default should be a policy that is complete on its own.

## Baselines

Four policies behind one interface, which is the one abstraction in this repository that earns its place: comparing them is the point of the project.

| Policy | How it picks a victim |
| --- | --- |
| `lru` | Highest `nowUS - lastAccess` among the sample. |
| `lfu` | Lowest `accesses` among the sample. No aging, marked as such. |
| `s3fifo` | Three FIFO queues of key hashes plus a two-bit counter: a small queue holding one tenth of capacity, a main queue, and a ghost queue of recently evicted hashes. |
| `lrb` | Highest learned score among the sample. |

S3-FIFO ([SOSP 2023](https://dl.acm.org/doi/10.1145/3600006.3613147)) is the interesting baseline.
Most objects in a skewed workload are one-hit wonders, and it exploits that directly: a new object enters the small queue, and leaves at the front unless it was accessed again, in which case it is promoted to main.
The main queue is CLOCK's second chance applied to a FIFO.
The ghost queue remembers evicted hashes so an object that comes back once is admitted straight to main.

It is a strong baseline and it needs no model, no trainer and no registry, which is why it is the default.

## Why an opt-in TTL does not invalidate the MIN comparison

A TTL and Belady's MIN are incompatible in principle.
MIN assumes an object stays useful until its next access; a TTL says the object stops being useful at a wall-clock instant regardless.
A trace scored under MIN with TTLs in play is measuring two different eviction mechanisms and attributing both to the policy.

The resolution is that TTLs never appear on the measurement path:

- `PutRequest.ttl_seconds` is per-call and defaults to zero.
- `CACHE_DEFAULT_TTL` defaults to `0`, meaning no default TTL.
- `loadgen` only ever calls `Get`, and read-through admissions carry the node's default TTL, which is zero.

So in every configuration where a hit ratio is compared against MIN, entries leave only by eviction or `Delete`, and "gap to Belady MIN" remains a statement about the policy.
Set `CACHE_DEFAULT_TTL` to something non-zero and that stops being true, which is why the variable is documented that way in `.env.example`.

`Stats` counts expirations separately from evictions for the same reason: evictions mean the cache is too small, expirations mean the data was too old, and only the first is a capacity problem.

## Further reading

- Bélády, *A study of replacement algorithms for a virtual-storage computer*, IBM Systems Journal, 1966.
- Song, Berger, Li, Lloyd, *Learning Relaxed Belady for Content Distribution Network Caching*, NSDI 2020.
- Yang, Zhang, Qiu, Yue, Vinayak, *FIFO queues are all you need for cache eviction*, SOSP 2023.
