# Contributing

## Getting set up

You need Go (the version in `go.mod`), Python 3.11 or newer, and Docker if you want to run the cluster.

```sh
make build           # compile every service into ./bin
make test            # unit tests with the race detector
make -C trainer venv # create the trainer virtualenv and install it
```

On macOS the LightGBM wheel needs OpenMP, which Homebrew does not install by default:

```sh
brew install libomp
```

## Running the whole thing

```sh
cp .env.example .env
make up        # the cluster, Prometheus and Grafana, then wait for health
make bench     # replay a Zipfian workload and report hit ratio and tail latency
make train     # fit a model from the captured traces and publish it
make bench     # again, to see what the model changed
make down
```

`make up-dev` is the same stack with the traces and models directories bind-mounted into the repo and each cache node's gRPC port published, which is what you want when you are debugging one node.

## Before you open a pull request

```sh
make lint          # go vet plus golangci-lint
make test
make proto-check   # regenerates the stubs and fails if the committed ones are stale
make -C trainer lint
make -C trainer test
```

CI runs all of these plus `govulncheck`, `gosec`, `pip-audit`, CodeQL, and a Compose integration run with a hit-ratio floor and a p99 ceiling.

If you touch a workflow, run `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` with `shellcheck` installed (`brew install shellcheck`).
Without it actionlint silently skips every `run:` block, which is where the mistakes are, and the check passes locally and fails in CI.

## Two contracts that will not fail loudly on their own

**The feature layout is shared between Go and Python.**
`internal/features/features.go` fills the vector at serving time and `trainer/belady_trainer/columns.py` names the columns at training time.
If they disagree the model still loads and still evaluates, reading one column wherever it was trained to read another.
`TestNamesMatchTheTrainer` parses the Python source and fails the Go build, so change both sides in the same commit.

**The Go evaluator understands one shape of LightGBM tree.**
`internal/model` accepts numeric two-way splits with default missing handling and nothing else.
`trainer/belady_trainer/train.py` pins the parameters that would produce anything else, and `test_the_dump_only_uses_nodes_the_go_evaluator_understands` checks the dump.
If you add a booster parameter, check what it does to the dump.

## Generated code

The protobuf stubs are committed, both the Go ones under `gen/` and the Python ones under `trainer/belady/`.
The toolchain versions are pinned in the Makefiles and in `trainer/pyproject.toml`, because the generator version is recorded in the output and an unpinned bump shows up as a spurious drift failure.
Never hand-edit generated files; run `make proto` and `make -C trainer proto`.

## Style

Small commits, each one green, with a Conventional Commits subject: `feat(cache):`, `fix(trainer):`, `docs:`, `chore(deps):`.

Comments explain the non-obvious and nothing else.
The ring buffer's memory ordering, the flattened tree indexing, the Belady boundary labelling, and the bounded-load hashing math all earn one; a comment that restates a function signature does not.
Mark a deliberate simplification with a `ponytail:` comment that names both the ceiling it accepts and the upgrade path, so the next person knows it was a choice.
The prose explanations live in `docs/`, one sentence per line.

## Measurements

If a change is meant to make something faster or raise the hit ratio, put the numbers in the pull request, from `make bench` or `make bench-micro`, with the configuration you used.
A hit-ratio win that costs more latency than it saves is not a win, and the arithmetic for that is three lines long.
Report the result you got, including when it is worse than you hoped.
