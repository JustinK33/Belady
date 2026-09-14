# Operations

## Two stacks, and how to pick one

**`make up-min`** is two containers: one cache node and a gateway with the HTTP API on.
No origin, no registry, no trainer, no Prometheus, no Grafana.
The policy is S3-FIFO, which needs no model.
This is the stack for *using* a cache.

```sh
export HTTP_AUTH_TOKEN=$(openssl rand -hex 32)
make up-min
```

**`make up`** is the full stack: gateway, three cache nodes, registry, origin, Prometheus and Grafana, with the learned policy and trace capture on.
This is the stack for *measuring* a cache.

```sh
cp .env.example .env
make up
make bench     # replay a workload, report hit ratio and tail latency
make train     # fit a model from the captured trace and publish it
make bench     # again, to see what the model changed
```

`make up-dev` layers `deploy/compose.dev.yaml` on top, which bind-mounts `traces/` and `models/` into the host tree, publishes each cache node's gRPC port, and turns segment rotation down to 512 KiB so traces appear in seconds rather than minutes.

`make down` tears down the full stack and its volumes; `make down-min` the minimal one.
`make help` lists everything.

Both stacks are `read_only: true` with `cap_drop: [ALL]`, `no-new-privileges`, a 16 MiB `/tmp` tmpfs and `init: true`.

### Why the health wait is a script and not a Compose healthcheck

The service images are distroless.
They contain no shell and no `curl`, so a Compose `healthcheck` has nothing to run.
`scripts/wait-for-health.sh` polls each container's debug port from the host instead, which is the reason those ports are published at all.
It takes service names as arguments, so the minimal stack waits on the two containers it runs rather than timing out on the six it does not.

## Configuration

Environment variables only.
No config files, so an image can never carry a secret, and `.env.example` documents every key with a non-secret placeholder.

`internal/config` has one rule worth knowing: a missing **required** value exits with status 2, and a **malformed** value warns and falls back to the default.
A typo in `CACHE_SHARDS` must not take a node out of rotation; a missing `CACHE_NODES` must.

Byte sizes accept `b`, `k`/`kb`/`kib`, `m`/`mb`/`mib`, `g`/`gb`/`gib`, `t`/`tb`/`tib`.
Durations are Go duration strings: `250ms`, `30s`, `10m`, `1h30m`.

### Cache node

| Variable | Default | Notes |
| --- | --- | --- |
| `GRPC_ADDR` | `:8081` | |
| `DEBUG_ADDR` | `:9090` | `/metrics`, `/healthz`, pprof |
| `NODE_ID` | hostname | Appears in `served_by` and in trace segment names |
| `ORIGIN_ADDR` | unset | **Set means read-through; unset means cache-aside.** No default on purpose |
| `REGISTRY_ADDR` | unset | Only consulted when the policy is `lrb` |
| `CACHE_POLICY` | `s3fifo` | `lru`, `lfu`, `s3fifo`, `lrb`. An unknown value is fatal |
| `CACHE_CAPACITY` | `256MiB` | Split evenly across shards |
| `CACHE_SHARDS` | `256` | Rounded **up** to a power of two |
| `CACHE_SAMPLE_SIZE` | `8` | Eviction candidates drawn per victim |
| `CACHE_DEFAULT_TTL` | `0` | `0` means entries leave only by eviction or `Delete` |
| `MODEL_BOUNDARY` | `10m` | Must match the model's `boundary_seconds` or the model is refused |
| `TRACE_ENABLED` | `false` | Compose turns it on for the full stack |
| `TRACE_DIR` | `/var/lib/belady/traces` | |
| `TRACE_SAMPLE_DENOMINATOR` | `16` | One key in N, by key hash |
| `TRACE_RING_CAPACITY` | `8192` | Per shard |
| `TRACE_SEGMENT_BYTES` | `32MiB` | Rotation on size |
| `TRACE_SEGMENT_MAX_AGE` | `1m` | Rotation on age |
| `TRACE_FLUSH_INTERVAL` | `1s` | Ring drain cadence |
| `TRACE_BATCH_SIZE` | `4096` | Records per `AccessBatch` |

### Gateway

| Variable | Default | Notes |
| --- | --- | --- |
| `CACHE_NODES` | **required** | Comma-separated `host:port`. Missing is exit 2 |
| `GRPC_ADDR` | `:8080` | |
| `HTTP_ADDR` | unset | Empty means the REST surface is off |
| `HTTP_AUTH_TOKEN` | unset | **Required whenever `HTTP_ADDR` is set** |
| `HTTP_TIMEOUT` | `5s` | Upstream deadline per HTTP request |
| `RING_REPLICAS` | `256` | Virtual nodes per member |
| `RING_LOAD_FACTOR` | `1.25` | Bounded-load ceiling as a multiple of the mean |
| `MAX_INFLIGHT` | `4096` | Concurrent upstream requests |

### Registry, origin, loadgen

`registry` takes `MODEL_DIR`, default `/var/lib/belady/models`.

`origin` takes `ORIGIN_LATENCY` (2ms), `ORIGIN_JITTER` (1ms), `ORIGIN_MIN_SIZE` (512), `ORIGIN_MAX_SIZE` (65536) and `ORIGIN_SIZE_ALPHA` (1.5), the Pareto shape.

`loadgen` takes `TARGET_ADDR`, `REQUESTS`, `KEYSPACE`, `CONCURRENCY`, `WARMUP`, `ZIPF_S`, `SEED`, plus the two gate thresholds `MIN_OBJECT_HIT` and `MAX_P99`.
Zero on either gate means report only.
`ZIPF_S` must be greater than 1.

All services take `LOG_LEVEL` (`debug`, `info`, `warn`, `error`) and `LOG_FORMAT` (`json` or `text`).

They also take `PROFILE_CONTENTION`, default `false`, which arms the runtime's mutex and block samplers.
`/debug/pprof/mutex` and `/debug/pprof/block` exist either way, so with this off they return an empty profile that reads as "no contention" when it actually means "not measured".
It is off by default because both samplers add work to every lock acquisition and every blocking operation in the process, which is also why a run with it on must not be the run you quote throughput or p99 from.

## Tuning

### Capacity

`CACHE_CAPACITY` counts **cached value bytes only.**
Entry metadata, gRPC buffers, the map itself and the Go heap all live in the same container and none of them are counted.

A cache node warns at startup when `GOMEMLIMIT` is set and is less than twice the configured capacity.
It warns rather than refusing, because an operator overriding this deliberately is a legitimate case.
The rule of thumb behind the factor of two: set `GOMEMLIMIT` to the container limit, and `CACHE_CAPACITY` to about half of it.

An object larger than one shard's byte budget can never be admitted.
With `CACHE_CAPACITY=64MiB` and `CACHE_SHARDS=256` a shard holds 256 KiB, so a 512 KiB object is rejected on every write.
`rejections` in `Stats` is where that shows up, and the fix is fewer shards or more capacity.

### Shards

`CACHE_SHARDS` is rounded up to a power of two, because the shard index is the low bits of the key hash and a mask is cheaper than a modulo.
Asking for 100 gets you 128, and the node logs the value it actually used.

More shards means less lock contention and smaller per-shard capacity.
Since eviction samples within a shard, very small shards also mean the sample is drawn from a smaller population, so the victim is chosen from less choice.
32 to 256 covers everything sensible; the default of 256 assumes a large node and `make up` overrides it to 32 for three small ones.

### The Belady boundary

Not a hyperparameter.
It has to sit inside the range of reuse times the workload actually produces, and the way to find out is to ask:

```sh
make -C trainer boundary        # or: docker compose ... run --rm trainer boundary
```

which prints the p50, p75, p90 and p99 reuse times in the captured trace.
Start at the median.

`MODEL_BOUNDARY` must be the same value at the trainer and at every cache node, which is why they read the same variable.
A mismatch is a refused model with a clear log line, not a silently wrong prediction.
The floor is 1 s, because `ModelMeta.boundary_seconds` is whole seconds.

### Sample size

`CACHE_SAMPLE_SIZE` trades victim quality against eviction cost, linearly on the model path: 8 candidates is 8 model evaluations.
8 is the default and is what every measurement here used.
Going to 16 roughly doubles the learned policy's eviction cost, which [03-performance.md](03-performance.md) shows is already the expensive half of the trade.

### Trace sampling

`TRACE_SAMPLE_DENOMINATOR` samples **keys**, not requests: one key hash in N, so every sampled key's history is complete.
The consequence is that the sampled fraction of *requests* swings run to run depending on whether the hottest keys landed in the sample.
Turn it to 1 for a short smoke run where you need every record; leave it at 16 for anything long.

## Training a model

```sh
make train              # on the host, from ./traces
make train-compose      # inside the cluster, reading the traces volume
```

### When a segment becomes trainable

A cache node writes to `<node>-<timestamp>.partial` and publishes by renaming it to `.trace`.
The trainer only reads `.trace`, which is what makes it safe to train from a directory the cache is still writing to.

A segment rotates on **three** triggers:

1. It reaches `TRACE_SEGMENT_BYTES`.
2. `TRACE_SEGMENT_MAX_AGE` elapses.
3. The node shuts down cleanly.

Until one of those fires, the directory looks empty to the trainer.
This is by far the most common "the trainer found no traces" cause, and both the Go and Python readers say so explicitly rather than reporting an empty directory:

> no .trace segments in traces, but 3 .partial file(s) are still open: a segment publishes when it reaches TRACE_SEGMENT_BYTES, when TRACE_SEGMENT_MAX_AGE elapses, or when the node shuts down cleanly

For a short run, turn both bounds down.
The integration workflow uses `TRACE_SEGMENT_BYTES=256KiB` and `TRACE_SEGMENT_MAX_AGE=5s`.

A zero-length segment is deleted rather than published.
A truncated tail, which means the writer died mid-frame, is dropped with a warning and everything before it is still used.

### What the trainer prints

```json
{
  "boundary_seconds": 600,
  "rows": 217283,
  "positive_rate": 0.0759,
  "holdout_auc": 0.985,
  "trees": 31,
  "dropped_censored": 6856,
  "top_features": ["recency_ms", "age_ms", "delta_0", "reuse_rate", "size_bytes"]
}
```

Read these in this order:

- **`positive_rate`** first. Outside 2% to 98% the trainer refuses to fit and prints the reuse-time quantiles instead. That is the boundary being wrong, not the model.
- **`holdout_auc`** second, and remember the holdout is split chronologically, not at random. Below about 0.7 the model is not ranking usefully and a boundary closer to the median is the first thing to try.
- **`trees`** third, because it is the inference budget. Early stopping usually lands around 30. Much more than 35 and eviction goes over its microsecond budget; see [03-performance.md](03-performance.md).
- **`dropped_censored`** last, as a sanity check. A large fraction means the trace is too short relative to the boundary.

### Confirming the rollout

The model reaches the nodes on its own; there is nothing to restart.

```sh
curl -s localhost:9201/metrics | grep belady_model_
```

`belady_model_loads_total` should go up by one on every node, and `belady_model_load_failures_total` should stay at zero.
A non-zero failure count is a refused model, and the node's log says why: wrong format, wrong feature count, wrong boundary, or a digest mismatch.

`Stats.model_version` is the authoritative answer to "is this actually running a model", and it is empty until one installs.
A benchmark labelled `lrb` with an empty `model_version` is sampled LRU.

## Reading the metrics

Prometheus at `localhost:9091`, Grafana at `localhost:3000`, both provisioned by `deploy/observability/`.
Every service exports on its own debug port; the mapping is in `.env.example`.

| Metric | Read it for |
| --- | --- |
| `belady_cache_hits_total`, `belady_cache_misses_total` | Object hit ratio |
| `belady_cache_hit_bytes_total`, `belady_cache_miss_bytes_total` | Byte hit ratio, which diverges from the object ratio whenever sizes are skewed |
| `belady_cache_evictions_total` | Capacity pressure |
| `belady_cache_expirations_total` | TTL pressure. Not the same problem |
| `belady_cache_rejections_total` | Objects too large for a shard |
| `belady_cache_objects`, `belady_cache_bytes_used`, `belady_cache_bytes_capacity` | Occupancy |
| `belady_cache_evict_ns_mean` | The learned policy's price |
| `belady_cache_trace_written_total`, `belady_cache_trace_dropped_total` | Whether the model was trained on a complete view of the traffic |
| `belady_model_loads_total`, `belady_model_load_failures_total` | Rollout |
| `belady_rpc_duration_seconds{method,code}` | Latency and error rate, per method |
| `belady_rpc_panics_total` | Should be zero. Any value is a bug |
| `belady_gateway_routed_total{node}` | Routing balance. A skew here means bounded loads are spilling |
| `belady_gateway_inflight` | Distance from `MAX_INFLIGHT` |

The cache counters are gauges collected on scrape rather than updated on the request path.
The store already counts them under a lock it was holding anyway, so the scrape reads a snapshot and the hot path never touches a metric.

Each process uses its own `prometheus.Registry` rather than the default one, so a stray library import cannot publish metrics nobody asked for.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `no .trace segments ... .partial file(s) are still open` | No segment has rotated yet. Turn `TRACE_SEGMENT_BYTES` and `TRACE_SEGMENT_MAX_AGE` down, or stop the node cleanly. |
| Trainer exits 2 with "too one-sided to rank candidates with" | The boundary is outside the workload's reuse times. Run `boundary` and use a value near the median. |
| `belady_model_load_failures_total` climbing | Read the node log. Almost always `MODEL_BOUNDARY` differing between trainer and node. |
| `Stats.policy` is `"mixed"` | Nodes are running different policies. Any hit-ratio comparison across the cluster is meaningless until that is fixed. |
| Every `Put` returns `admitted: false` | The object is larger than one shard's budget. Fewer shards or more capacity. |
| `429` from the HTTP API | The gateway's inflight ceiling filled and the request's deadline expired while queued. Raise `MAX_INFLIGHT` or reduce client concurrency. |
| Gateway exits 2 immediately | Either `CACHE_NODES` is unset, or `HTTP_ADDR` is set without `HTTP_AUTH_TOKEN`. |
| `bytes_used` stays high while the hit ratio falls | Expired entries holding capacity. Expiry is lazy by design; see the note below. |
| A node logs a trace write failure once at startup | The trace directory is not writable by uid 65532. See the volume note below. |
| MIN scores *below* the policy in `loadgen` | The object-count conversion from byte capacity is off. MIN cannot actually be beaten. |

### The volumes and uid 65532

The service images are distroless `nonroot`, so every process runs as uid 65532 on a read-only root filesystem.
The only writable paths are the `/tmp` tmpfs and whatever volume is mounted.

The images ship an **empty `/var/lib/belady/traces` and `/var/lib/belady/models`, owned by 65532.**
That is not decoration.
Docker seeds a fresh named volume from the image's contents at that path, including ownership, and that is the only way a distroless non-root container gets a writable named volume without an init container or a root entrypoint.
Without it the first run of the full stack has every node logging a write failure once a second while serving traffic perfectly, which is exactly what happened the first time this ran in CI.

A **bind mount** does not get that treatment: the host directory's ownership wins.
`make up-dev` bind-mounts `traces/` and `models/`, which works on macOS because Docker Desktop's file sharing layer papers over ownership, and on a Linux host needs either a `user:` override or a `chown 65532` on the host directory.

### Lazy TTL expiry

An expired entry is dropped when something reads it, never by a background sweeper.
That keeps the write path free of a timer and keeps the read path honest about never serving stale data.

The cost is that a keyspace written with a TTL and then never read again holds capacity until the policy evicts it, which on a mostly-idle cache can be a long time.
The signal that this has become a real problem is `belady_cache_bytes_used` staying high while the hit ratio falls, and the upgrade is an active sweep.
The marker is on `Entry.expires` in `internal/cache/entry.go`.

## Profiling

pprof is on every service's debug port, separately from the gRPC port, so the debug surface can be firewalled without touching the data path.

```sh
go tool pprof -http=: http://localhost:9201/debug/pprof/profile?seconds=30
go tool pprof -http=: http://localhost:9201/debug/pprof/heap
curl -o trace.out 'http://localhost:9201/debug/pprof/trace?seconds=5'
```

The first of those is the outstanding piece of work on this project: [03-performance.md](03-performance.md) has an eviction cost about 7x higher than the microbenchmarks predict and no profile to explain it.
