# ADR 0002: gRPC for the internal APIs

Status: accepted.

## Context

This is a cache, so the obvious question is why the internal calls are gRPC rather than something either simpler or faster.
The question is worth answering properly, because the wrong answer is visible in the results: the request path is where every number in [03-performance.md](../03-performance.md) is produced, and the headline is a break-even measured in microseconds per request.

There are four internal call patterns:

- `Cache.Get`, `Put`, `Delete`, `Stats`, on the hot path, gateway to node.
- `Origin.Fetch`, on the miss path.
- `Registry.PublishModel` and `GetModel`, bulk transfers of a few hundred kilobytes.
- `Registry.WatchModels`, a long-lived server stream that is the entire hot-reload mechanism.

The polyglot boundary from [ADR 0001](0001-go-services-python-trainer.md) also has to be crossed: the trainer publishes models from Python.

## Decision

gRPC for everything between services.
A REST adapter on the gateway only, added later, off unless `HTTP_ADDR` is set, as the external surface for humans and clients that should not need generated stubs.

## Consequences

One `.proto` set generates both the Go services and the Python trainer client, so the polyglot contract is machine-checked rather than documented.
Three of the registry's four methods are streams, and `WatchModels` gets flow control, keepalive, and clean reconnect semantics without any of it being hand-written.
Message size caps, concurrent stream limits and panic recovery are set once in `internal/grpcx` and inherited by every binary.

The cost is that the hot path pays HTTP/2 framing and protobuf encoding per request, and that a curl-level debugging session needs `grpcurl` or the REST port.

`loadgen` is a gRPC `Cache` client, which means the transport is part of the measurement apparatus.
Changing it invalidates comparability with every recorded run, so this decision is load-bearing for the results and not only for the code.

## What was given up

**Not REST.** REST was never the competing option for internal calls; it is strictly worse here, adding per-request JSON encoding to a path whose whole point is microsecond accounting, and turning the model watch into hand-rolled SSE or long-polling with reconnect and backoff logic.
It earned a place as the *external* surface, which is a different job.

**A purpose-built binary protocol** is the alternative that could have won.
RESP, or something memcached-shaped: a length-prefixed binary framing over a raw TCP connection, no HTTP/2, no protobuf, request and response parsed with a hand-written reader.
It would very likely beat gRPC on the hot path rather than lose to it, which is exactly why it is the honest rejected alternative.

It was not chosen because it buys throughput at the cost of the two things this project is actually built on: one schema shared with the Python trainer, and streaming semantics for model rollout that would have to be reinvented.
It remains a real option and it is on the roadmap.
It competes for attention with the CPU profile that [03-performance.md](../03-performance.md) says the eviction number needs first, and profiling before optimising is the correct order.
