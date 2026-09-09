# Trainer pipeline

2026-09-09

## TL;DR

Built step 12 of the plan, the Python trainer, which closes the loop: the cluster now trains a model from traces it sampled itself, publishes it over gRPC, and all three cache nodes install it without a restart.
Ran the whole thing end to end against real binaries and measured the learned policy against LRU.
It wins on hit ratio by 0.62 points and costs 10x more per eviction, which is roughly break-even against a 200 µs origin.
Committed as `ef161b8`; steps 13-16 (Docker/Compose, CI, docs, diagram) are still open.

## Changes

| File | What it does now |
| ---- | ---------------- |
| `trainer/belady_trainer/samples.py` | Replays entry state from a trace and emits labelled snapshots at a random offset into each inter-access interval |
| `trainer/belady_trainer/train.py` | LightGBM fit with a time-based holdout; pins off the parameters that would make the dump unparseable by `internal/model` |
| `trainer/belady_trainer/publish.py` | Streams a model to the registry with digest, feature count, boundary and training metrics |
| `trainer/belady_trainer/__main__.py` | `train` and `boundary` subcommands; parses Go-style durations so `MODEL_BOUNDARY` is shared with the nodes |
| `internal/features/features_test.go` | `TestNamesMatchTheTrainer` parses `columns.py` and fails the Go build if the two column layouts drift |
| `trainer/tests/` | 30 tests covering the segment reader, entry replay, labelling, censoring, and the node shapes the Go evaluator accepts |
| `trainer/{Makefile,README.md,pyproject.toml}` | Targets for venv/proto/test/lint/train; README documents the libomp prerequisite on macOS |
| `README.md` | Added measured results and a "what building this taught me" section |

## Metrics

Three nodes, 32 shards, 6 MiB each, zipf s=1.1 over 50k keys, 400k requests at concurrency 64, 200 µs origin, Apple M4. Cold start per policy.

| Measurement | LRU | Learned (31 trees) |
| ----------- | --- | ------ |
| Object hit ratio | 0.8732 | 0.8794 |
| Byte hit ratio | 0.8645 | 0.8705 |
| Gap to Belady MIN (0.9428) | -6.96 pts | -6.33 pts |
| Evict cost | 815 ns/victim | 8548 ns/victim |
| p99 / p99.9 | 3.31 / 4.55 ms | 3.42 / 5.76 ms |
| Throughput | 51,967 req/s | 51,211 req/s |

Training: 224,139 sampled records over 8.5 s, 217,283 rows after dropping 6,856 censored, positive rate 5.38%, holdout AUC 0.9847, 31 trees / 961 nodes, parsed by each node in 0.5-1.4 ms.

## Notes

- The live eviction penalty is 7.7 µs but the isolated microbenchmarks put it at 55 ns/victim (274 vs 219). Two orders of magnitude unexplained; worth a profile before writing `docs/03-performance.md`.
- Two bugs came from running it rather than reading it: the delta shift read off the front of a trace shorter than the 8-entry history, and a zero-length interval (two clients touching one key in the same microsecond) asked numpy for a random integer in an empty range. Both now have tests.
- Segments only become trainable when they rotate or the node shuts down, so a large `TRACE_SEGMENT_BYTES` with modest traffic yields nothing to train on. Belongs in the operations doc.
- I spent time this session convinced the repo had been wiped. It had been renamed from `newproj` to `Belady`; the module path and the GitHub repo are still `newproj`, which is worth reconciling.
