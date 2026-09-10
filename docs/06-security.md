# Security

What the project defends against, what it deliberately does not, and where the trust boundaries actually are.
[SECURITY.md](../SECURITY.md) is the reporting policy and the in-scope list; this document is the reasoning behind it.

## Threat model

The assumed deployment is a single trust domain: one host, or one private network, running the whole cluster.
That assumption is doing a lot of work, and everything below either follows from it or is a gap it leaves open.

| Actor | Can reach | Assumed |
| --- | --- | --- |
| A cache client | The gateway's REST port, and its gRPC port | Untrusted. Sends whatever it likes. |
| Anything on the network | Every gRPC port, including `registry` and `cachenode` directly | Trusted, because nothing checks. This is the gap. |
| The trainer | `Registry.PublishModel` | Untrusted for the content it publishes, trusted for the fact that it may publish. |
| A published model | Every cache node that installs it | Untrusted input, parsed by hand. |
| The operator | Environment variables, volumes | Trusted. |

The two things worth attacking are the model path, because a model is untrusted input that every node parses and then executes decisions from, and the registry's filesystem handling, because a version string becomes a filename.

## The trust boundaries

There are three, and each one has code that exists only to hold it.

**The trainer to the registry.**
`validate` in `internal/registry/registry.go` is the boundary.
The trainer is a separate process in another language, so nothing it asserts about a blob is taken on trust: the digest is recomputed over the bytes that actually arrived, the declared size must match, the format must be `lightgbm-text`, the feature count must be non-zero, and the Belady boundary must be non-zero.
A model that fails any of those never reaches disk.

**A version string to a filename.**
`validVersion` rejects anything that is not a bare name, which means empty, longer than 64 bytes, or containing `/`, `\` or `.`.
It runs on both paths that turn a version into a filename: publishing, where it arrives from the trainer, and `GetModel`, where it arrives straight from an untrusted request.
It only ran on the publish path until recently, which is written up under [What has actually gone wrong](#what-has-actually-gone-wrong) below.

**A model file to the evaluator.**
`internal/model` parses LightGBM's text dump, which is untrusted input by design, and it is the largest hand-written parser in the tree.
The parser's job is to fail rather than to produce a tree that indexes out of bounds later, so structural checks happen at parse time and the hot path has none.
A cache node also refuses a model whose feature count disagrees with its own build, or whose Belady boundary disagrees with its `MODEL_BOUNDARY`, because both mismatches are silent at runtime: the model loads, evaluates, and scores against a feature layout or a horizon it was never fit for.

## What is hardened

**Containers.**
Every image runs non-root on a read-only root filesystem with all capabilities dropped, from a base image pinned by digest.
The only writable paths are the volumes, and `/var/lib/belady` ships in the image owned by uid 65532 so a new named volume inherits that ownership.

**Configuration.**
Environment only.
There is no config file, so an image cannot carry a secret, `internal/config` exits 2 on a missing required value, and `.env` is gitignored.
`.env.example` documents every key with a non-secret placeholder.

**gRPC limits.**
`internal/grpcx` sets them in one place rather than per binary: an 8 MiB receive cap, 4096 concurrent streams per connection, keepalive with a minimum client ping interval so pings cannot be used as a load amplifier, and a recovery interceptor that turns a handler panic into `Internal` plus a counter rather than a dead process holding a few gigabytes of cache.

**The REST surface.**
`HTTP_AUTH_TOKEN` is mandatory whenever `HTTP_ADDR` is set, compared in constant time, and the gateway exits 2 rather than starting an unauthenticated public port.
A refusal to start is the point: the failure mode of an optional token is an open cache that looks like it is working.

**Supply chain.**
`govulncheck`, `gosec`, `pip-audit` and CodeQL run on every change.
Dependabot watches Go modules, pip, Actions and the base images.
Every GitHub Action is pinned to a commit SHA and every workflow declares a minimal `permissions:` block.
The generated protobuf stubs are committed and CI fails on drift, so a change to the wire contract cannot arrive as an invisible side effect of someone's local generator version.

## The two gaps that are open on purpose

**gRPC has no transport security.**
Not a fallback and not a switch: `grpcx.Dial` passes `insecure.NewCredentials()` unconditionally, and the servers are constructed without TLS credentials at all.
Traffic between the gateway, the nodes, the origin and the registry is plaintext, and a model in transit is plaintext.
mTLS is the production path and the marker is on `grpcx.Dial`; the work is certificate issuance and rotation rather than the dozen lines that install the credentials, which is why it is not in v1.

**Nothing authenticates between services.**
Any process that can reach a cache node's port can read, write and delete any key.
Any process that can reach the registry can publish a model that every node will install, which is the more interesting one: it is remote influence over eviction decisions on the whole cluster, bounded only by what `validate` checks.
The REST surface is the exception rather than the rule here, and its token protects the gateway's public port, not the fabric behind it.

Both gaps have the same mitigation, and it is an operational one: **do not expose a Belady cluster to a network you do not control.**
`SECURITY.md` lists their absence as out of scope for reports, because a documented gap is a decision and not a finding.

## What has actually gone wrong

One real bug, found while writing this document rather than by a scanner, which is the honest provenance.

`Registry.GetModel` took the version from the request and passed it to `filepath.Join` without validation.
Publishing had always checked that a version was a bare name, and two `//nolint:gosec` comments asserted the check had already happened, which was true on the publish path and false on the read path.
`GetModel{version: "../secret"}` therefore read and streamed a `.model` file from outside `MODEL_DIR`.
The extension suffix bounded it to files ending in `.model` and `.meta`, and the missing authentication above meant anyone who could reach the registry could do it.

The fix extracts the existing rule as `validVersion` and applies it to both paths.
`TestGetModelRejectsTraversal` in `internal/registry` is the regression test, and it fails against the previous code.

The lesson worth keeping is not "validate input", which everyone already agrees with.
It is that a suppression comment claiming an invariant holds is a claim nobody re-checks when a second caller appears, and the second caller is where it stopped being true.

## Reporting

Through GitHub's private vulnerability reporting, per [SECURITY.md](../SECURITY.md).
The in-scope and out-of-scope lists live there, and the short version is that remote crashes, memory-safety or accounting bugs, model-sandbox escapes, registry path handling and secret leakage all count, while the absence of TLS and authentication does not.
