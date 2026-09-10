# ADR 0006: Values on the Go heap, not an arena or an mmap tier

Status: accepted.

## Context

A cache holds bytes.
How those bytes are allocated determines GC pressure, fragmentation, and how much of the configured capacity is actually usable.

The standard objections to putting cached values on a garbage-collected heap are real: every cached object is a live pointer the collector must trace, and a cache is by definition mostly-live data with a high churn rate.

## Decision

Individually allocated `[]byte`, handed out to callers without copying.
Metadata lives in the same `Entry` struct as the value pointer.
No arena, no slab of pre-allocated blocks, no off-heap allocation, no disk tier.

## Consequences

The store is `2^k` independently locked shards with all counters on the shard, so capacity accounting is exact and cheap and there is no cache-wide lock.

Handing the value out without copying is the part that matters for allocation counts: the hit path allocates nothing, asserted by a `-benchmem` benchmark.

GC does trace every cached object.
The measured runs use 6 MiB per node, where this is not visible.
The marker on `Entry` in `internal/cache/entry.go` names the trigger for revisiting: pprof showing GC dominating rather than intuition about it.

## What was given up

**An arena or slab allocator.**
This is the alternative that could have won, and it was rejected on a specific observation rather than on principle.
grpc-go's codec copies the value into the response message regardless, so the value's lifetime already ends at the copy.
An arena would therefore buy allocator and GC wins only, with no reduction in copying, in exchange for a use-after-free class of bug: eviction would have to prove no in-flight response still references the slab region.
Trading a whole class of memory-safety bug for GC pressure that has not been measured as a problem is the wrong order of operations.

**An mmap-backed disk tier.**
Listed as out of scope in the roadmap, not as later work.
A second tier changes what the eviction comparison means: the policy would be choosing where an object lives rather than whether it survives, and "gap to Belady's MIN" stops being a statement about eviction quality.

## Related

The delta history is a fixed `[8]uint32` on the entry, shifted with one `copy` per access, for a related reason: 32 bytes is a single cache line, and a ring buffer would move the index arithmetic onto feature extraction, which runs eight times per eviction instead of once per access.
