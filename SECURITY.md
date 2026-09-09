# Security policy

## Scope and honest expectations

Belady is a research and portfolio project.
It is built to production hygiene standards but it has never run in production, and two things in particular are deliberately unfinished:
gRPC runs without TLS in the default configuration, and there is no authentication on any service.
Both are documented as such in [docs/06-security.md](docs/06-security.md) rather than hidden.
Do not expose a Belady cluster to a network you do not control.

## Reporting a vulnerability

Report privately through GitHub's [private vulnerability reporting](https://github.com/JustinK33/Belady/security/advisories/new) on this repository.
Please do not open a public issue for anything exploitable.

Include the version or commit, the configuration, and the smallest reproduction you have.
I will acknowledge within seven days and aim to have either a fix or a written explanation of why it is not one within thirty.

## What counts

In scope:

- Remote crashes or hangs reachable through the gRPC APIs, including malformed protobuf, oversized messages, and stream abuse.
- Memory-safety or accounting bugs in the cache, the slab, or the trace ring buffers.
- Anything that lets a published model escape its sandbox, including the LightGBM text parser in `internal/model`, which parses untrusted input by design.
- Path traversal or arbitrary write through the model registry's version handling.
- Secrets leaking into logs, metrics, or trace segments.

Out of scope:

- The absence of TLS and authentication in the default configuration, which is a known and documented gap rather than a finding.
- Denial of service by simply sending more load than the configured limits allow.
- Findings that require the operator to be already root on the host.
- The `origin` service, which is a test fixture and makes no security claims at all.

## What the project already does

- Every container runs non-root on a read-only root filesystem with all capabilities dropped, from a base image pinned by digest.
- Configuration is environment-only; no secret is ever written to a tracked file, and `.env` is gitignored.
- gRPC servers cap message size and concurrent streams, and recover from handler panics rather than taking the process down.
- The model registry validates every published blob before storing it: digest, declared feature count, and a non-zero Belady boundary.
- CI runs `govulncheck`, `gosec`, `pip-audit`, and CodeQL on every change, and Dependabot watches Go modules, pip, Actions, and the container images.
- Every GitHub Action is pinned to a commit SHA, and every workflow declares a minimal `permissions:` block.
