# trainer

Offline trainer for the learned eviction policy.
It reads the access traces the cache nodes sampled, derives Relaxed Belady labels, fits a LightGBM model, and publishes it to the registry.

Nothing here runs on a request path.
The cache nodes are already watching the registry, so publishing is the whole deployment step.

## Prerequisites

Python 3.11 or newer.

On macOS, LightGBM's wheel links against OpenMP and does not bundle it, so `import lightgbm` fails with `Library not loaded: @rpath/libomp.dylib` until you install it:

```sh
brew install libomp
```

Linux wheels are self-contained and need nothing extra.

## Usage

```sh
make venv                    # create .venv and install the package
make test                    # unit tests
make boundary                # report the reuse times in a captured trace
make train                   # fit a model and publish it to the registry
make train-only              # fit a model, write model.txt, publish nothing
```

`make train` accepts the same environment variables the cache nodes read:

| Variable | Default | Meaning |
| --- | --- | --- |
| `TRACE_DIR` | `../traces` | Directory of `*.trace` segments to train from |
| `MODEL_BOUNDARY` | `10m` | Relaxed Belady boundary, as a Go duration |
| `REGISTRY_ADDR` | `localhost:8082` | Registry to publish to |
| `MODEL_OUT` | `model.txt` | Where to write the LightGBM dump |

`MODEL_BOUNDARY` is deliberately the same variable the cache nodes read.
A node refuses a model whose boundary disagrees with its own configuration, because "will not be reused within 10 minutes" and "will not be reused within 30 seconds" are different predictions wearing the same name.
Sharing the variable makes that check a safety net rather than a routine failure.

## Choosing the boundary

The boundary is a property of the workload, not a tuning knob.
Set it far above the reuse times in the trace and every object is labelled "will be reused", set it far below and none are, and in both cases the model trains happily and then ranks candidates at random.

`make boundary` prints the reuse-time quantiles so you can see where it should sit:

```
reuse time quantiles (seconds):
  p50        0.0614
  p75        0.1902
  p90        0.5231
  p99        3.8104
```

If the labels come out too one-sided, `make train` refuses to fit and tells you the median reuse time rather than producing a model that looks fine by accuracy and is useless by ranking.

## Layout

| Path | Role |
| --- | --- |
| `belady_trainer/columns.py` | The feature layout, mirrored from `internal/features/features.go` |
| `belady_trainer/dataset.py` | Reads the length-delimited `AccessBatch` segment files |
| `belady_trainer/samples.py` | Replays entry state and emits labelled snapshots |
| `belady_trainer/train.py` | The LightGBM fit and the constraints the Go evaluator imposes |
| `belady_trainer/publish.py` | Streams a model to the registry |
| `belady/` | Generated protobuf stubs, regenerate with `make proto` |

## The two contracts with Go

Both are places where a mistake is silent, so both are checked rather than documented and hoped for.

**Feature layout.**
`columns.py` and `internal/features/features.go` declare the same fourteen columns in the same order.
Swap two and the model still loads, still evaluates, and reads `size_bytes` wherever it was trained to read `recency_ms`.
`TestNamesMatchTheTrainer` in `internal/features` parses `columns.py` and fails the Go build on drift.

**Node shape.**
`internal/model` is a hand-written parser for LightGBM's text dump, and it supports exactly one node: a numeric threshold with a two-way branch.
Categorical splits, a missing-value branch, linear leaves, and multiclass all make the model unloadable, so `train.py` pins off the parameters that produce them and `test_the_dump_only_uses_nodes_the_go_evaluator_understands` asserts the emitted dump stayed inside that subset.

See `docs/02-learned-eviction.md` for why the features are what they are and what the boundary trick buys.
