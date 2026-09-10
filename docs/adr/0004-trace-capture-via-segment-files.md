# ADR 0004: Traces are segment files, not a stream to the trainer

Status: accepted.

## Context

Training needs access traces from a live cluster: which key was touched, when, how big it was, and whether it hit.
The cache node is the only place that knows.

The obvious design is a gRPC stream from each node to whatever collects traces.
The proto even has a trace service defined for it.

## Decision

Each shard writes records into a lock-free ring.
A background goroutine drains the rings and appends to a segment file in `TRACE_DIR`, rotating on size.
A segment becomes visible by atomic rename.
The trainer reads finished segments off disk.

## Consequences

The trainer is not a dependency of anything on the write path.
It can be absent, crashed, or mid-upgrade and the cache does not notice, which is the correct relationship for an offline job.
Pushing to a stream would have inverted it: a slow or missing consumer would apply backpressure to a cache node serving traffic.

Atomic rename means a reader can never observe a partial segment, so the trainer needs no coordination, no locking and no "is this file finished" heuristic.

Traces survive a node restart, so a training run can use data captured before the last deploy.

Records are dropped rather than blocking when a ring fills, counted in `belady_cache_trace_dropped_total`.
No request is ever slowed down to make room for its own telemetry.

The cost is a disk dependency and a volume to manage, plus the operational question of when enough trace exists to be trainable, which [05-operations.md](../05-operations.md) answers.

## What was given up

**A gRPC stream to a collector**, which is what the trace service in the proto was originally for.
Rejected for the backpressure inversion above: it makes an offline consumer able to affect serving latency, and the failure mode is worst exactly when the cluster is busiest and the traces are most interesting.

**Writing rows directly in a trainable format**, such as Parquet or CSV.
Rejected because the write path is under a shard lock and a 24-byte pointer-free record is the cheapest thing that can be handed to a ring buffer.
Formatting belongs in the trainer, which has time.
