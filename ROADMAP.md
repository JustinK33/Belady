# Roadmap

What is built, what is next, and what has been deliberately left out.
Kept in the repo rather than in an issue tracker so the plan and the code go stale together, which at least makes the drift visible.

Last reviewed 2026-09-14.

## Where the project stands

The end-to-end loop works and is measured.
The cluster serves traffic, samples its own access traces, trains a model from them, publishes it, and all three cache nodes install it without a restart.
The learned policy beats sampled LRU on hit ratio by 0.66 points and costs 4.5 times more per eviction; the numbers and the break-even arithmetic are in the [README](README.md), and the method behind them is in [docs/03-performance.md](docs/03-performance.md).

| Step | What it delivered | State |
| --- | --- | --- |
| 1-2 | Repository, module, lint config, env-only config, observability | done |
| 3 | gRPC contract for cache, origin, registry and traces | done |
| 4 | Sharded store with per-shard counters and sampled eviction | done |
| 5 | LRU, LFU and S3-FIFO baselines with a shared conformance suite | done |
| 6 | `origin` and `cachenode`, the first traffic-serving path | done |
| 7 | `gateway` with consistent hashing under bounded loads | done |
| 8 | `loadgen` with Zipfian replay scored against Belady's MIN | done |
| 9 | LightGBM text parser and flat tree evaluator | done |
| 10 | Feature extraction, lock-free trace sampling, the LRB policy | done |
| 11 | Model registry with watch-driven hot reload | done |
| 12 | Python trainer: labelling, training, publishing | done |
| 13 | Docker images, Compose stack, Prometheus and Grafana | done |
| 14 | GitHub Actions, Dependabot, security and contributor docs | done |
| 15 | `docs/` prose, nine ADRs, the architecture diagrams | done |
| 16 | Final measured pass, numbers into the docs | done |
| 17 | Usable mode: REST surface, per-entry TTL, optional origin, two-container stack | done |

Steps 13 and 14 were written without a running Docker daemon and without a push to GitHub, so they sat at "done, unverified" until the first real Actions run.
That run found four bugs no local check had, which is the argument for pushing early rather than reading YAML harder: `sha256sum *` unquoted in the release job, a repository description containing a double quote that buildx parses as CSV and rejects on every image build, seven reachable stdlib advisories on the `go` directive, and a fresh Docker named volume being root-owned so every node logged a write failure once a second while serving happily.
All four workflows are green on `main` now, and the integration run is the first proof the whole loop works in containers rather than on a laptop: 133,631 training rows sampled from a live cluster, holdout AUC 0.9749, the model installed by all three cache nodes with zero load failures, and a second replay served under it at 0.8919 object hit against a Belady MIN of 0.9381.

Step 17 is out of numerical order because it was not in the plan.
It came from asking what it would take to actually use this rather than only measure it, and the answer turned out to be small: `ORIGIN_ADDR` was the only setting with no usable default, so an unset one now means cache-aside instead of a misconfiguration; entries can carry a TTL; the gateway serves a REST surface behind a mandatory bearer token; and `make up-min` brings the whole thing up as two containers.
The default policy moved from LRB to S3-FIFO in the same pass, because LRB with no trained model is sampled eviction under a misleading name.
None of it changes the measurement path: `loadgen` never sets a TTL and `CACHE_DEFAULT_TTL` is zero, so entries still leave only by eviction, which is what makes "gap to Belady MIN" a statement about the policy.

## Step 15, as built

The six prose documents and nine ADRs are in [docs/](docs/), indexed by [docs/README.md](docs/README.md).

Two things came out differently from the plan.

The diagram is two Mermaid blocks in `docs/01-architecture.md` rather than an Excalidraw scene.
Mermaid renders inline on GitHub, needs no external tool to open, and changes in the same diff as the prose it explains.
The plan's requirement was a strict grid with no overlapping arrows or labels, and that survived the format change: the first topology layout fanned out to three cache nodes and produced crossing arrows, so it was redrawn as one collapsed node, which is also the more honest picture given that all three nodes are identical.

There are nine ADRs rather than eight, because trace capture via segment files was already being cross-referenced as 0004 by the prose and is a genuine decision with a losing alternative.

Writing `docs/06-security.md` found a real bug: `Registry.GetModel` passed an unvalidated request-supplied version to `filepath.Join`, so a version of `../secret` read a model file from outside `MODEL_DIR`.
Fixed in `a8db709` with `validVersion` applied to both the publish and read paths, and a regression test.
Documentation that only restates the code cannot find anything; documentation that has to state the invariant out loud can.

## Step 16, as built

The confirmation run happened, and the interesting part was that the documented commands did not reproduce the documented configuration.

Five separate defects in the reproduce block, each of which would have sent a reader somewhere other than the recorded numbers.
The block said `make up && make bench` while the prose above it recorded the pass as native rather than containerized.
Four load-bearing parameters differed from the committed defaults and were set by no documented command: 400,000 requests against `REQUESTS=200000`, 50,000 keys against `KEYSPACE=100000`, 6 MiB per node against `CACHE_CAPACITY=64MiB`, and 200 µs origin latency against `ORIGIN_LATENCY=2ms`.
`MODEL_BOUNDARY` was a fifth unrecorded parameter, and the documented default of `10m` cannot fit this workload at all.
`make train` after `make up` cannot see any traces, because they live in a Compose volume, so the step is `make train-compose`.
And after choosing a boundary the cache nodes must be recreated, or they refuse the new model on a boundary mismatch and the run silently measures the fallback policy instead of the learned one.

Everything in the results table was re-measured against a pinned, executed block, and the numbers moved: 0.8702 object hit for LRB against 0.8636 for LRU, and 8343 ns/victim against 1867.
Throughput was dropped from the table rather than restated, because four runs of the same configuration spanned 17% on a machine where six containers share eight cores, while hit ratio held to 4 basis points and eviction cost to 4%.
Reporting a number that noisy next to numbers that stable would have implied all of them were measurements.

## Known open items

These are real, they are not blocked on anything, and they are ordered by how much they bother me.

**Tree evaluation is 74% of eviction cost, and nothing has been done about it yet.**
This item used to read "the live eviction penalty is about 7x worse than the microbenchmark predicts, and that remainder is unexplained".
It is now accounted for to within about 1.4x, and the write-up is under [The gap between the microbenchmark and the live number](docs/03-performance.md#the-gap-between-the-microbenchmark-and-the-live-number).
The short version is that most of the gap was never about the model: LRU inflates 9.8x on the same trip from a native single-shard benchmark to a containerized three-node cluster, and the original comparison never ran the baseline through both ends.

Both of the fixes this item used to imply are now ruled out by measurement.
Descheduling under the shard lock is falsified: with contention profiling on for a full run, the mutex profile attributes 4.13 ms to the shard lock and all of it to reads, with `insert` and `evictOne` absent, the block profile attributes nothing to the `cache` package, and a CPU profile puts `lrb.Victim` at the same on-CPU time the counter reports as wall time.
Map-range sampling and the `Entry` layout are 10% and 16% of `Victim` respectively, so optimizing either would move a tenth of the cost.

What is left is the 74%: `model.Raw` over 13 trees.
The lever with the most behind it is evaluating all eight candidates in one pass instead of eight, because a fixed feature vector is 1.35x faster than a varying one purely on branch prediction (`BenchmarkRawVaryingFeatures`), and batching is what would let the predictor see one walk instead of eight.
That is a hot-path change and is deliberately not in this pass.

**The Belady boundary must be at least one second, and the documented default is unusable.**
`ModelMeta.boundary_seconds` is whole seconds, the registry rejects zero, and a cache node refuses any model whose boundary disagrees with its own `MODEL_BOUNDARY`.
The default is `10m`, and on the benchmark workload it labels 0.0000% of rows "beyond boundary", so the trainer correctly refuses to fit: p50 reuse time is 0.0002 s, p90 is 0.0351 s, and p99 is 2.79 s.
The smallest legal boundary, 1 s, sits between p90 and p99 and is what the measured pass uses, which works but is a coincidence rather than a design.
`git log -S` on the labelling check confirms it predates the earlier recorded pass, so that pass cannot have used the documented default either, and the parameter it did use was never written down.
Either the field becomes milliseconds or the default changes to something a real trace can satisfy; leaving a default that always fails is the one option that is clearly wrong.

**The integration workflow's training phase is still the most likely thing to be flaky.**
It trains a model from a trace captured seconds earlier, so if the smoke run is too short or too fast the labels come out one-sided and the trainer refuses to fit, which is the correct behaviour and a red build.
The one passing run so far had a positive rate of 0.0759 against a `MIN_CLASS_RATE` floor of 0.01, which is comfortable but is a single sample.
If it proves unstable, the fix is a longer smoke run, not a lower floor.

**TTL expiry is lazy, so an expired entry holds its bytes until something touches it.**
Expiry is resolved on read rather than by a background sweep, which keeps the write path free of a timer and the read path honest about never serving stale data.
The cost is that a keyspace written with a TTL and then never read again occupies capacity until the policy evicts it, which on a mostly-idle cache could be a long time.
The signal to add an active sweep is `bytes_used` staying high while the hit ratio falls, and the marker is in `internal/cache/entry.go`.

**Dev-mode Compose bind mounts assume Docker Desktop's permission mapping.**
`make up-dev` bind-mounts `traces/` and `models/` into containers running as uid 65532.
That works on macOS, where the file sharing layer papers over ownership, and will need a `user:` override or a chown on a Linux host.

## Later, in rough priority order

**Score all eight candidates in one batched pass.**
The profile says tree evaluation is 74% of eviction cost, so this is the only change with a factor in it rather than a percent.
It is a hot-path change to `internal/model` and `internal/cache/lrb.go`, and the guard is that eviction must stay at zero allocations.

**A hit-ratio-versus-cost sweep instead of a single data point.**
The learned policy's value depends almost entirely on what a miss costs, so the honest chart is hit ratio and throughput against origin latency, from tens of microseconds to tens of milliseconds.
That turns "roughly break-even here" into a curve with a crossing point.

**Replay against a real CDN trace.**
Everything so far is Zipfian synthetic, and Zipf is kind to policies that lean on frequency.
The Wikipedia and Tencent traces used in the LRB paper would make the comparison to published numbers meaningful.

**Simulate residency when sampling training rows.**
Snapshots are currently taken without asking whether a real cache would still hold the object, so some rows describe entries that would already have been evicted.
It is a deliberate simplification with a marked ceiling in `samples.py`, and the trigger for revisiting it is feature importances that start to look shaped by the policy that captured the trace.

**Continuous retraining rather than a manual `make train`.**
The registry already supports rollout and the nodes already watch it, so the missing piece is a scheduler and a rule for when a new model is better than the one in production.
That rule is the hard part and it needs shadow scoring, not a cron entry.

**A purpose-built binary protocol on the hot path.**
Named as the rejected alternative in [ADR 0002](docs/adr/0002-grpc-for-internal-apis.md), because it is the option that could genuinely beat gRPC rather than lose to it: RESP or something memcached-shaped, length-prefixed framing over raw TCP, no HTTP/2 and no protobuf on `Cache.Get`.
It is below the batched-scoring work on purpose.
Eviction costs 8.3 µs against a measured p50 of 1.84 ms, so the transport is a larger share of a request than eviction is, but nothing has measured which part of it: the honest version of this item is that measurement first, so what fraction of per-request time is framing and encoding?
Note also that `loadgen` is a gRPC client, so any change here has to keep the measurement path comparable or restate every number in `docs/03-performance.md`.

**mTLS between services.**
gRPC has no transport security at all today, not merely off by default: `grpcx.Dial` passes insecure credentials unconditionally.
The work is certificate issuance and rotation rather than the dozen lines that install the credentials, which is why it is not in v1.
See [docs/06-security.md](docs/06-security.md).

**Kubernetes manifests.**
Deferred on purpose in [ADR 0008](docs/adr/0008-compose-over-kubernetes.md).
Compose is the right substrate for a project whose point is measurement, and a Helm chart would add operational surface without adding a single number to the results.

## Out of scope

Not "later", but decided against for this project.

- An mmap-backed disk tier. The cache is a memory substrate and adding a second tier changes what the eviction comparison even means.
- Multi-region replication.
- A web UI. Grafana covers the observability need and a bespoke dashboard is a different project.
- Alternative model runtimes such as ONNX or CGO-linked LightGBM. The hand-rolled evaluator exists precisely because the sub-microsecond budget is the interesting constraint; delegating it removes the point.
