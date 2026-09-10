# ADR 0003: Sampled eviction, not a global priority queue

Status: accepted.

## Context

An eviction policy has to answer "which resident object should go" when the cache is full.
The textbook structure is a priority queue over every resident object, which gives the exact minimum of whatever the priority function is.

The learned policy's value proposition is a *fractional* improvement in victim quality: in the measured run, 0.62 points of object hit ratio over sampled LRU.
That number is what any implementation cost has to be weighed against.

## Decision

Sample `CACHE_SAMPLE_SIZE` candidates from the shard's map, eight by default, score each, and evict the argmax.
No structure is maintained between evictions.

## Consequences

A hit updates the entry in place and touches nothing else.
With a heap, every hit becomes an O(log n) sift under the shard lock, which is a cost paid on the *common* path to improve the *rare* one.
The measured workload runs at 0.118 evictions per request ([03-performance.md](../03-performance.md)), so requests outnumber evictions by about eight to one and that trade is the wrong way round.

Eviction cost is bounded and predictable: eight feature extractions and eight model evaluations, independent of cache size.
The eviction loop allocates nothing, which is only achievable because there is no structure to rebalance.

The policy interface is the same shape for LRU, LFU, S3-FIFO and LRB, so the conformance suite in `internal/cache` runs all four against the same assertions, and the baselines are honest comparisons rather than differently-shaped code.

The victim is not the true minimum.
For a random sample of size k, the expected quantile of the best candidate is 1/(k+1), so eight candidates land around the 11th percentile of evictability rather than the 0th.
This is the same trick Redis uses for its sampled LRU, and the measured gap to Belady's MIN absorbs it.

## What was given up

**A global priority queue**, exact and O(log n) per update.
Rejected on the arithmetic above: it moves cost onto the hit path to buy victim quality that the hit-ratio numbers say is worth under a point.

**A CLOCK or segmented-LRU approximation**, which is cheaper still.
Rejected because the whole point is to evaluate a *learned* score per candidate, and an approximation that never materialises a comparable score per object has nowhere to put the model.

## Known ceiling

Sampling draws from Go's randomised map range start, which skews slightly toward fuller hash buckets rather than being uniform over entries.
It is marked in `internal/cache/shard.go`.
The bias is small and in an unhelpful-but-not-wrong direction, and fixing it properly means maintaining an index, which is the structure this decision exists to avoid.
