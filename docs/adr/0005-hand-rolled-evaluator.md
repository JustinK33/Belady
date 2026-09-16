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
Rejected for the same call-overhead reason, plus a large native dependency in every cache node image for a workload whose fitted models run to a few dozen shallow trees.

Both are listed as out of scope in the roadmap rather than as later work, because the sub-microsecond budget is the interesting constraint of this project and delegating inference removes the thing being studied.

**A batched evaluator, scoring the whole eviction sample in one pass over the trees.**
This is the one that looked like a clear win and is not.
The eight candidates are independent and each walk is a chain of two dependent loads per level, so interleaving them should give the load unit several chains at once, and a CPU profile puts `Raw` at 74% of eviction cost.

Four shapes were prototyped and measured before anything was added to `Raw`: tree-outer with candidate-inner, level-lockstep, and hand-unrolled scalar locals at widths 4 and 8.
At the live shape the best was 1.04x to 1.09x, and **once the feature vectors stop repeating often enough for the branch predictor to memorise the paths, every batched shape is slower than eight sequential `Raw` calls.**
The arithmetic said so beforehand: 65 node visits in 60 ns is about 3 cycles per visit, which a strictly serial two-load chain cannot reach, so `Raw` was already overlapping several chains and there was less parallelism left to find than the profile suggested.

Rejected on the measurement, in [03-performance.md](../03-performance.md#batching-the-eight-candidates-into-one-pass-which-does-not-work).
`Raw` stays one candidate at a time, which is also what keeps it the reference the exactness test is written against.
