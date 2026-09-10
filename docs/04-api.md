# API

Two surfaces over the same code.
gRPC is the contract between services and the one every internal call uses.
A REST adapter sits in front of it on the gateway, off by default, so the cache is usable without generating stubs first.

Definitions live in `api/belady/v1/`.
Generated Go stubs are committed under `gen/`, generated Python stubs under `trainer/belady/`, and CI fails on drift in either.
That is [ADR 0009](adr/0009-commit-generated-protobuf.md).

## The `Cache` service

```protobuf
service Cache {
  rpc Get(GetRequest) returns (GetResponse);
  rpc Put(PutRequest) returns (PutResponse);
  rpc Delete(DeleteRequest) returns (DeleteResponse);
  rpc Stats(StatsRequest) returns (StatsResponse);
}
```

Implemented by **both** the gateway and the cache nodes.
The gateway is a routing proxy in front of the same interface, so the same client works against either, and a cache node can be exercised directly in a test without a gateway in the way.

### Get

`GetResponse` carries `value`, `found`, `source` and `served_by`.

`found = false` means the object is in neither the cache nor origin.
A node with no `ORIGIN_ADDR` has no origin to consult, so there a miss is simply not found; see the two modes in [01-architecture.md](01-architecture.md).

`source` distinguishes `SOURCE_CACHE` from `SOURCE_ORIGIN`, which is what makes a read-through fetch visible to a client.
`served_by` names the cache node that handled the request, so routing can be inspected from the outside rather than inferred from gateway metrics.

An empty key is `InvalidArgument`.

### Put

```protobuf
message PutRequest {
  string key = 1;
  bytes value = 2;
  uint32 ttl_seconds = 3;
}
```

`ttl_seconds = 0` means the node's `CACHE_DEFAULT_TTL` applies, which is itself zero by default, and zero there means the entry only ever leaves by eviction or `Delete`.

Seconds rather than milliseconds, deliberately.
A TTL finer than the policy's own decision interval is not something a cache can honour meaningfully, and the field being coarse is what makes the HTTP layer's rejection of sub-second TTLs a clean error rather than a rounding surprise.

`PutResponse.admitted = false` is **not** an error.
It means the write was handled correctly and the admission policy declined, most often because the object is larger than a single shard's byte budget.
A client that does not care can ignore it.

### Delete

Fanned out to every node by the gateway, not routed.
Bounded loads mean a key is not guaranteed to have lived on exactly one node, so a delete has to reach all of them.
It is cheap, and the alternative is a stale value resurfacing when load shifts back.

`existed` is the OR across nodes.
A failure on any node is `Unavailable` naming the node, rather than a partial success reported as success.

### Stats

The counters needed to score a policy.

| Field | Meaning |
| --- | --- |
| `node_id`, `policy`, `model_version` | `model_version` is empty until a model is actually installed, so a hit-ratio result can never be misattributed to a model that never loaded. |
| `hits`, `misses` | Object hit ratio is `hits / (hits + misses)`. |
| `hit_bytes`, `miss_bytes` | Byte hit ratio is `hit_bytes / (hit_bytes + miss_bytes)`. The two diverge whenever object sizes are skewed, which is the usual case. |
| `admissions`, `rejections` | Admissions accepted and refused. |
| `evictions` | Objects removed to make room. |
| `expirations` | Objects removed because a TTL passed. Kept separate on purpose: evictions mean the cache is too small, expirations mean the data was too old, and only the first is a capacity problem. |
| `objects`, `bytes_used`, `bytes_capacity` | Current occupancy. |
| `trace_sampled`, `trace_dropped` | A non-zero drop count means a trace ring filled, not that requests were harmed. |
| `evict_ns_mean` | Mean nanoseconds to choose a victim, including one clock read. This is the number the learned policy has to keep small enough to be worth its hit-ratio gain. |

From the gateway, counters are summed across nodes and `evict_ns_mean` is averaged unweighted, which is close enough when nodes are the same size and is labelled a mean rather than a percentile for that reason.
`policy` becomes the literal string `"mixed"` when nodes disagree, because a blended hit ratio across two policies is meaningless and should look wrong rather than plausible.

## The `Origin` service

```protobuf
service Origin {
  rpc Fetch(FetchRequest) returns (FetchResponse);
}
```

One call, `key` in and `value`/`found` out.
This is the interface to implement if you want to point Belady at something real: set `ORIGIN_ADDR` at any service that serves it.

The bundled `origin` is a fixture.
Latency is derived from the key hash rather than a random source, so two runs of the same workload see the same miss costs, and object sizes follow a Pareto distribution by inverse-transform sampling, because real object sizes are heavy-tailed and that is exactly what makes byte hit ratio diverge from object hit ratio.
It returns `found = true` for every key, which means a read-through cluster backed by it can never produce a `NotFound`.

## The `Registry` service

```protobuf
service Registry {
  rpc PublishModel(stream PublishModelRequest) returns (PublishModelResponse);
  rpc GetModel(GetModelRequest) returns (stream GetModelResponse);
  rpc WatchModels(WatchModelsRequest) returns (stream ModelEvent);
  rpc ListModels(ListModelsRequest) returns (ListModelsResponse);
}
```

Streaming in both directions because a LightGBM text dump grows with tree count and easily passes a comfortable unary message size.
The chunk size on both sides is 256 KiB.

`PublishModel` is a client stream whose **first message must carry `meta`**; every later message carries a `chunk`.
Metadata twice, or a chunk before any metadata, is `InvalidArgument`.
The accumulating buffer is bounded by `meta.size_bytes` rather than by trusting the stream to end, because an unbounded append on an incoming stream is a memory-exhaustion primitive.

`GetModel` with an empty `version` means "the newest".
The response opens with `meta` and then streams chunks.

`WatchModels` takes `since_version` and sends an event for every model newer than it, starting immediately if the registry is already ahead.
That is the whole reason it is a watch and not a poll: a node that restarts converges without waiting for the next training run.
The implementation keeps no subscriber registry; every publish swaps `latest` and closes a `changed` channel, so each watcher gets exactly one wakeup per publish and a watcher that disappears leaks nothing.

`ListModels` returns newest first, `limit = 0` meaning all.
A model whose metadata is unreadable is skipped with a warning rather than failing the listing, because the operator needs the rest of the list in order to diagnose it.

### `ModelMeta`

| Field | Notes |
| --- | --- |
| `version` | Sortable, e.g. `20260909T120000Z`. Lexical order is deployment order. Validated as a bare filename, because it becomes one. |
| `format` | Only `lightgbm-text` is accepted. |
| `size_bytes`, `sha256` | Both re-checked against the bytes that actually arrived, on both the publish and the fetch side. |
| `created_unix` | Filled in by the registry when the publisher leaves it zero. |
| `feature_count` | Required. A cache node cannot check compatibility without it. |
| `boundary_seconds` | Required and non-zero. The prediction is meaningless without the boundary it was trained against, and a node refuses a model whose boundary disagrees with its own `MODEL_BOUNDARY`. |
| `metrics` | Free-form `map<string,string>`: `auc`, `rows`, `positive_rate`, `trees`. Logged when the model installs. |

Validation in `internal/registry/registry.go` is a trust boundary, not a formality: the publisher is a different process in a different language, so everything it asserts about the blob is checked rather than assumed.

## Traces

`api/belady/v1/trace.proto` defines **no service**.
Cache nodes write access traces as length-delimited `AccessBatch` messages to segment files on a shared volume, and the trainer reads them offline.

```protobuf
message AccessRecord {
  uint64 key_hash = 1;
  int64 timestamp_us = 2;
  uint32 size_bytes = 3;
  bool hit = 4;
}
```

`key_hash` rather than the key, so the plaintext key cannot reach training data by construction.
Microseconds rather than nanoseconds, which is enough to order accesses and compute deltas and halves the varint width.

Protobuf is not self-delimiting, so each batch is prefixed with a varint byte count.
There are three implementations of that framing: the Go writer, the Go reader, and the Python reader.
Deliberately, because the alternative is the trainer depending on a Go binary.

The rationale for files over a stream is [ADR 0004](adr/0004-trace-capture-via-segment-files.md).

## Deadlines, limits and errors

**Deadlines are the client's.**
No service invents one for an inbound request.
Cache nodes pass the caller's context straight through to origin, and the gateway passes it to the node, so one client deadline bounds the whole chain.
The one place a deadline is created is the HTTP adapter, because an HTTP client has no way to express one: `HTTP_TIMEOUT`, 5 s by default.

**Limits.**

| Limit | Value | Set in |
| --- | --- | --- |
| Max inbound message | 8 MiB | `grpcx.MaxRecvBytes`, on both server and client |
| Max concurrent streams per connection | 4096 | `grpcx.MaxConcurrentStreams` |
| Gateway concurrent upstream requests | `MAX_INFLIGHT`, 4096 | Semaphore in `cmd/gateway` |
| Keepalive | ping every 30 s, 10 s timeout, 10 s minimum enforced on clients | `internal/grpcx` |
| HTTP request body | 8 MiB | `http.MaxBytesReader` |
| HTTP read header timeout | 5 s | `http.Server` |

**Codes.**

| Code | When |
| --- | --- |
| `InvalidArgument` | Empty key. A malformed publish stream. Metadata that fails validation. |
| `NotFound` | `GetModel` for a version that does not exist, or when nothing has been published. |
| `ResourceExhausted` | The gateway's inflight ceiling was full and the client's deadline expired while queued. A distinct code, so a dashboard can tell overload from slowness. |
| `Unavailable` | No cache nodes configured. A per-node failure during a `Delete` or `Stats` fan-out. |
| `Internal` | A recovered handler panic, or a filesystem failure in the registry. |
| `DeadlineExceeded` | Propagated from the caller's context. |

A panicking handler is recovered by an interceptor, counted in `belady_rpc_panics_total`, and returned as `Internal`.
A panic taking a cache node down would be a much worse failure than one request failing.

## The REST surface

Gateway only, and off unless `HTTP_ADDR` is set.
Cache nodes stay gRPC-only so the one reachable surface is also the one that owns rate limiting.

It is a thin adapter: every handler calls the same `server` methods gRPC calls, so routing, single-flight, the inflight ceiling and the metrics are shared rather than reimplemented.

### Authentication

`HTTP_AUTH_TOKEN` is **mandatory** whenever `HTTP_ADDR` is set.
The gateway exits with status 2 rather than starting, because an open HTTP port in front of a cache is a data leak and defaulting to no auth is how that happens by accident.

```
Authorization: Bearer <token>
```

Compared with `subtle.ConstantTimeCompare`.
A plain `==` would leak the token's prefix through timing, which is cheap to avoid and awkward to explain afterwards.
A rejection is `401` with `WWW-Authenticate: Bearer realm="belady"`.

### Routes

| Method and path | Body | Response |
| --- | --- | --- |
| `GET /v1/keys/{key...}` | - | `200` with `application/octet-stream`, plus `X-Belady-Node` and `X-Belady-Source`. `404` when not found. |
| `PUT /v1/keys/{key...}` | The value, raw | `200` with `{"admitted": bool, "node": string}` |
| `DELETE /v1/keys/{key...}` | - | `200` with `{"existed": bool}` |
| `GET /v1/stats` | - | `200` with the cluster stats as JSON, including derived `object_hit_ratio` and `byte_hit_ratio` |

`{key...}` rather than `{key}`, so a slash-separated key like `user/42/profile` is one key and not a `404`.

`X-Belady-Source` is `cache`, `origin` or `unknown`, which is the cheapest possible way to see read-through working: a cold read reports `origin`, the next read of the same key reports `cache`.

### TTL

`?ttl=` takes a Go duration string: `30s`, `5m`, `1h30m`.

A TTL below 1 s is **rejected with `400`, not truncated.**
The wire field is whole seconds, so truncating would make `?ttl=100ms` silently mean "no TTL at all", which is the opposite of what was asked for.
Rejecting is the only behaviour that cannot surprise anyone.

An absent `ttl` means zero, which means the node's `CACHE_DEFAULT_TTL`, which is itself zero by default.

### Status mapping

| gRPC code | HTTP status |
| --- | --- |
| `InvalidArgument` | 400 |
| `NotFound` | 404 |
| `ResourceExhausted` | 429 |
| `Unavailable` | 503 |
| `DeadlineExceeded` | 504 |
| anything else | 500, and logged |

Only the codes these handlers can actually produce are mapped.
Anything else becomes a 500 and a log line, because guessing a friendlier status for an unexpected failure hides it.

A body larger than 8 MiB is `413`.
Every error body is `{"error": "<message>"}`.

### Example

```sh
export HTTP_AUTH_TOKEN=$(openssl rand -hex 32)
make up-min

auth="Authorization: Bearer $HTTP_AUTH_TOKEN"
curl -H "$auth" -X PUT --data-binary 'hello' 'localhost:8090/v1/keys/greeting?ttl=5m'
curl -H "$auth" -D - localhost:8090/v1/keys/greeting
curl -H "$auth" -X DELETE localhost:8090/v1/keys/greeting
curl -H "$auth" localhost:8090/v1/stats | jq .
```

The integration workflow runs a longer version of exactly this against a real cluster, including the 401 and the sub-second-TTL 400, so the table above is checked rather than described.
