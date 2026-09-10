# Roadmap

What is built, what is next, and what has been deliberately left out.
Kept in the repo rather than in an issue tracker so the plan and the code go stale together, which at least makes the drift visible.

Last reviewed 2026-09-09.

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
| 15 | `docs/` prose, eight ADRs, the architecture diagram | next |
| 16 | Final measured pass, numbers into the docs | next |
| 17 | Usable mode: REST surface, per-entry TTL, optional origin, two-container stack | done |

Steps 13 and 14 were written without a running Docker daemon and without a push to GitHub, so they sat at "done, unverified" until the first real Actions run.
That run found four bugs no local check had, which is the argument for pushing early rather than reading YAML harder: `sha256sum *` unquoted in the release job, a repository description containing a double quote that buildx parses as CSV and rejects on every image build, seven reachable stdlib advisories on the `go` directive, and a fresh Docker named volume being root-owned so every node logged a write failure once a second while serving happily.
All four workflows are green on `main` now, and the integration run is the first proof the whole loop works in containers rather than on a laptop: 133,631 training rows sampled from a live cluster, holdout AUC 0.9749, the model installed by all three cache nodes with zero load failures, and a second replay served under it at 0.8919 object hit against a Belady MIN of 0.9381.

Step 17 is out of numerical order because it was not in the plan.
It came from asking what it would take to actually use this rather than only measure it, and the answer turned out to be small: `ORIGIN_ADDR` was the only setting with no usable default, so an unset one now means cache-aside instead of a misconfiguration; entries can carry a TTL; the gateway serves a REST surface behind a mandatory bearer token; and `make up-min` brings the whole thing up as two containers.
The default policy moved from LRB to S3-FIFO in the same pass, because LRB with no trained model is sampled eviction under a misleading name.
None of it changes the measurement path: `loadgen` never sets a TTL and `CACHE_DEFAULT_TTL` is zero, so entries still leave only by eviction, which is what makes "gap to Belady MIN" a statement about the policy.

## Next

### Step 15: documentation and the diagram

The code carries only the comments that explain something non-obvious; the prose belongs in `docs/`.

- `docs/README.md` as an index.
- `docs/01-architecture.md`: the five services, the request path, where state lives, why the boundaries fall where they do.
- `docs/02-learned-eviction.md`: Belady's MIN, the Relaxed Belady boundary, the feature vector, how labels are derived, why snapshot offset matters more than the booster, and why an opt-in TTL does not invalidate the MIN baseline as long as the measured runs do not set one.
- `docs/03-performance.md`: the budgets, the allocation strategy, the microbenchmarks, and the measured cluster numbers with the method spelled out.
- `docs/04-api.md`: the gRPC contract, deadlines, message size caps, error semantics, and the REST surface alongside it - the route table, TTL semantics and why sub-second TTLs are rejected rather than truncated, and how gRPC codes map onto HTTP statuses.
- `docs/05-operations.md`: running it, the two modes and how to pick one, `HTTP_AUTH_TOKEN` and why a set `HTTP_ADDR` without one is a refusal to start, tuning capacity and shards, choosing the boundary, reading the metrics, when a trace segment becomes trainable, and why the images carry an empty `/var/lib/belady` owned by uid 65532 - Docker seeds a new named volume from the image, which is the only way a distroless non-root container can write to one, and a Linux bind mount via `make up-dev` still needs a `user:` override or a chown.
- `docs/06-security.md`: the threat model, what is hardened, and the two known gaps.
- Eight ADRs: the Go and Python split, gRPC over HTTP, sampled eviction over a global priority queue, a hand-rolled Go evaluator over CGO or ONNX, in-memory slab over mmap, consistent hashing with bounded loads, Compose over Kubernetes, and committing generated protobuf code.
- `docs/diagrams/architecture.excalidraw`: two scenes, black strokes only, on a strict grid with no overlapping arrows or labels.

### Step 16: the final measured pass

Run the whole thing once more from a clean state, paste the numbers into `docs/03-performance.md`, and update the README headline if they move.

## Known open items

These are real, they are not blocked on anything, and they are ordered by how much they bother me.

**The live eviction penalty is two orders of magnitude worse than the microbenchmark predicts.**
In isolation LRB eviction costs 274 ns/victim against LRU's 219 ns.
Live, under 64 concurrent clients, the same comparison was 8548 ns against 815 ns, and the containerised CI run measured 11,781 ns.
The microbenchmark measures a hot model in L1 against a working set that fits in cache and neither is true in the running system, but that is a hypothesis, not an explanation.
This needs a CPU profile of a cache node under load before `docs/03-performance.md` can honestly claim to explain the number.

**The Go module path is `github.com/JustinK33/newproj` and the repository is `Belady`.**
Reconciling it touches `go.mod`, every import in the tree, and the `go_package` option in all three protos, which means a mechanical commit that regenerates the stubs.
Worth doing before anyone else reads the code, and it is your call whether to do it now or after step 16.

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

**mTLS between services.**
Documented as the production path today with an insecure dev fallback that logs a warning.
The work is certificate issuance and rotation, which is why it is not in v1.

**Kubernetes manifests.**
Deferred to an ADR on purpose.
Compose is the right substrate for a project whose point is measurement, and a Helm chart would add operational surface without adding a single number to the results.

## Out of scope

Not "later", but decided against for this project.

- An mmap-backed disk tier. The cache is a memory substrate and adding a second tier changes what the eviction comparison even means.
- Multi-region replication.
- A web UI. Grafana covers the observability need and a bespoke dashboard is a different project.
- Alternative model runtimes such as ONNX or CGO-linked LightGBM. The hand-rolled evaluator exists precisely because the sub-microsecond budget is the interesting constraint; delegating it removes the point.
