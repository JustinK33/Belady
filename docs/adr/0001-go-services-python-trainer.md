# ADR 0001: Go services, Python trainer

Status: accepted.

## Context

The project needs two very different things from a language.

The serving path holds a shard lock a hundred thousand times a second, evicts with a sub-microsecond budget, and must allocate nothing on the hit path.
The training path fits a gradient-boosted tree over a few hundred thousand labelled rows, a few times a day, offline.

LightGBM's training implementation is a C++ library whose usable interface is Python.
Its inference format is a text dump that anything can read.

## Decision

Go for the five services and the load generator.
Python for the trainer, as a batch job that shares no process with anything serving.
The two sides meet at two contracts and nowhere else: the protobuf definitions in `api/`, and the feature column order.

## Consequences

The feature layout is the same contract written twice, in `internal/features/features.go` and `trainer/belady_trainer/columns.py`.
A mismatch is invisible at runtime, because the model loads and evaluates perfectly while reading `size_bytes` where it was trained to read `recency_ms`.
`TestNamesMatchTheTrainer` parses the Python from Go and fails the build on drift, and it has its own CI job because it is the one failure in this repository that is otherwise completely silent.

Two toolchains, two dependency ecosystems, two vulnerability scanners, two sets of pinned versions.
That cost is real and it is paid in `Makefile`, `trainer/pyproject.toml` and the CI matrix.

The trainer being a separate process is also what makes it optional.
Nothing serving depends on it, so a trainer that is not running, or fails to fit, changes nothing about the request path.

## What was given up

**Training in Go**, with a hand-written or third-party GBDT implementation.
This would have removed the second toolchain and the twice-written contract.
It was rejected because fitting a competitive gradient-boosted tree is a much harder and less interesting problem than the one this project is about, and because the published LRB results are LightGBM results, so a different implementation would have made the comparison to them meaningless.

**Serving in Python**, with the trainer's own model object.
Rejected on the numbers this project exists to measure: the eviction budget is hundreds of nanoseconds and the GIL is not a detail that can be worked around at that scale.
