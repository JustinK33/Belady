# Roadmap

What is built, what is next, and what has been deliberately left out.
Kept in the repo rather than in an issue tracker so the plan and the code go stale together, which at least makes the drift visible.

Last reviewed 2026-09-10.

## Where the project stands

The end-to-end loop works and is measured.
The cluster serves traffic, samples its own access traces, trains a model from them, publishes it, and all three cache nodes install it without a restart.
The learned policy beats sampled LRU on hit ratio by 0.62 points and costs roughly ten times more per eviction; the numbers and the break-even arithmetic are in the [README](README.md).

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
| 16 | Final measured pass, numbers into the docs | next |
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

## Next

### Step 16: the final measured pass

Mostly landed already: `docs/03-performance.md` carries a full measured pass with the method spelled out, and the microbenchmark figures in it reproduce within run-to-run noise.
What is left is a confirmation run from a clean state, and updating the README headline if the numbers move.

## Known open items

These are real, they are not blocked on anything, and they are ordered by how much they bother me.

**The live eviction penalty is about 7x worse than the microbenchmark predicts, and that remainder is unexplained.**
This item used to read "two orders of magnitude, unexplained" against microbenchmark figures of 274 ns/victim for LRB and 219 ns for LRU.
Both were superseded by the measured pass, which reproduces at 195 ns for LRU and 254 to 261 ns for LRB, and `docs/03-performance.md` explains most of the apparent gap: `BenchmarkEvict/lrb` installs a synthetic single-tree, 25-leaf model, so reading its result as a prediction for a 31-tree fit was the mistake.
Corrected, the prediction is ~1.2 µs/victim against 8.5 µs measured live, so the real discrepancy is about 7x rather than 30x.

The plausible causes are a hot model and a cache-resident working set in the microbenchmark against a 6 MiB working set of pointer-chased map entries live, plus `evict_ns_mean` including time the goroutine spent descheduled under contention.
Neither has been demonstrated, and this still needs a CPU profile of a cache node under load.
`/debug/pprof` is on every service's debug port, so the data is one command away; the work is doing it and reading it.

**The Belady boundary must be at least one second.**
`ModelMeta.boundary_seconds` is whole seconds, the registry rejects zero, and a cache node refuses any model whose boundary disagrees with its own `MODEL_BOUNDARY`.
On a workload whose median reuse time is milliseconds, the smallest legal boundary is already well above the median, which is workable but not obviously right.
Either the field becomes milliseconds or the operations doc explains why second granularity is the sensible floor.

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

**Profile and close the eviction gap.**
The most interesting engineering question the project has produced, and the answer changes what the README claims.

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
It is below the profiling work on purpose.
Optimising a transport before knowing where the eviction microseconds actually go is the wrong order, and the honest version of this item is a measurement first: what fraction of per-request time is framing and encoding?
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
