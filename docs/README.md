# Documentation

The code carries comments only where something is non-obvious.
Everything that needs a paragraph rather than a line lives here.

## Read in this order

| Document | What it answers |
| --- | --- |
| [01-architecture.md](01-architecture.md) | What the five services are, what a request does, where state lives, and why the boundaries fall where they do. |
| [02-learned-eviction.md](02-learned-eviction.md) | What Belady's MIN is, what the Relaxed Belady boundary buys, what the fourteen features are, and how a label is derived from a trace. |
| [03-performance.md](03-performance.md) | The latency budget, the allocation strategy, the microbenchmarks, and the measured cluster numbers with the method spelled out. |
| [04-api.md](04-api.md) | The gRPC contract and the REST surface in front of it: routes, TTL semantics, deadlines, size caps, error codes. |
| [05-operations.md](05-operations.md) | Running it, the two modes, tuning capacity and shards, choosing a boundary, reading the metrics, training a model. |
| [06-security.md](06-security.md) | The threat model, what is hardened, and the two gaps that are open on purpose. |
| [adr/](adr/) | Nine decision records, each one thing that could reasonably have gone the other way. |

## Diagrams

Both live in [01-architecture.md](01-architecture.md) as Mermaid, so they render inline and change in the same diff as the prose they explain: the service topology, and the sequence of a miss that triggers an eviction.

## Conventions in these documents

Every number that appears here was measured, and the command that produced it is given next to it.
Where something is a simplification with a known ceiling, the ceiling is named, and the code carries a matching `ponytail:` comment at the same place.
Where something is unexplained rather than explained, it says so; the open questions are collected in [ROADMAP.md](../ROADMAP.md).
