# ADR 0005: A hand-rolled tree evaluator, not CGO or ONNX

Status: accepted.

## Context

Eviction evaluates the model once per candidate, eight times per eviction, under the shard lock.
The budget is hundreds of nanoseconds for the whole eviction, which puts the per-evaluation budget in the tens of nanoseconds.

LightGBM ships a C++ inference library.
ONNX Runtime has Go bindings.
Both are well tested and neither is written by this project.

## Decision

Parse LightGBM's text dump in `internal/model` and evaluate it with hand-written Go over flat arrays.

One contiguous `[]node` of 16-byte nodes plus a `[]int32` of tree roots.
No pointers, no interface dispatch, no per-tree slice header.
A leaf is a negative index (`^v`), so the inner loop is a compare, a load and a branch, with no separate leaf array to chase.

## Consequences

The evaluation path is pure Go with no allocations and no cgo transition.
A cgo call costs tens to hundreds of nanoseconds on its own, which is the entire budget before any tree is walked, and it also pins an OS thread, which interacts badly with holding a shard lock.

The feature contract can guarantee no value is ever NaN, which is what allows a plain two-way branch per node with no missing-value handling.
That guarantee is only available because the same project owns both the extractor and the evaluator.

The build stays `CGO_ENABLED=0`, so the images are distroless static binaries and cross-compilation is free.

The cost is that this project owns a parser for someone else's format.
It is the largest hand-written parser in the tree and it takes untrusted input, which makes it a security surface documented in [06-security.md](../06-security.md).
It supports the subset of LightGBM that this project produces, and a model using unsupported features is a parse failure rather than a wrong prediction, which is the correct direction to fail.

## What was given up

**CGO-linked LightGBM.**
The reference implementation, exactly correct by construction.
Rejected because the cgo call overhead is comparable to the entire eviction budget, and because it would end the static build.

**ONNX Runtime.**
More portable and would support model types this evaluator does not.
Rejected for the same call-overhead reason, plus a large native dependency in every cache node image for a workload that is 31 trees deep.

Both are listed as out of scope in the roadmap rather than as later work, because the sub-microsecond budget is the interesting constraint of this project and delegating inference removes the thing being studied.
