# ADR 0008: Docker Compose, not Kubernetes

Status: accepted.

## Context

The cluster is five services plus Prometheus and Grafana, and it exists to be measured.
Every claim in the README comes from running the whole thing and reading numbers off it.

That means the substrate has one job: bring up a known topology reproducibly, on a laptop and in CI, with as little between the measurement and the process as possible.

## Decision

Docker Compose, in three stacks:

- `make up` for the full measured cluster: gateway, three cache nodes, origin, registry, Prometheus, Grafana.
- `make up-dev` for bind-mounted traces and models during development.
- `make up-min` for the two-container usable stack.

No Kubernetes manifests and no Helm chart.

## Consequences

The integration workflow runs the same stack CI runs and a developer runs, so "works on my machine" and "works in CI" are the same claim.

Three cache nodes are three entries in a YAML file, which is the right amount of ceremony for a fixed topology that exists to be replayed against.

Startup ordering is not needed, because `grpcx.Dial` is lazy and buffers the first RPC.
That removes health-check ordering, init containers and readiness gates from the problem entirely.

There is no autoscaling, no rolling deploy and no service discovery, and none of those are exercised by a measurement run.
Model rollout, which is the one thing that genuinely needs to happen without a restart, is handled by the registry's watch stream and not by the orchestrator.

The cost is that this is not a production deployment story.
Anyone wanting one has to write it, and the container images are the useful artifact for that: distroless, non-root, read-only root filesystem, configured entirely by environment variables, which is exactly what a Kubernetes deployment would need anyway.

## What was given up

**Kubernetes manifests, or a Helm chart.**
The realistic alternative and the one a reader may expect.
Rejected because it adds operational surface without adding a single number to the results, and because the properties Kubernetes provides are ones this project deliberately does not need: a cache node is disposable, holds no durable state, and is replaced rather than healed.

This is deferred rather than refused.
It is on the roadmap under later work, and the images are built so that it stays a small piece of work whenever someone wants it.

**Running the services as bare processes** under a process manager, which would remove Docker too.
Rejected because the measured numbers should come from the same artifact that ships, and because the container hardening in [06-security.md](../06-security.md) is part of what is being claimed.
