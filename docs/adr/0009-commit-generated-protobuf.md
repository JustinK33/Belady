# ADR 0009: Commit the generated protobuf stubs

Status: accepted.

## Context

The wire contract lives in `api/belady/v1/`.
It generates Go stubs for five services and Python stubs for the trainer, crossing the language boundary from [ADR 0001](0001-go-services-python-trainer.md).

The generated code can either be committed or produced at build time.

## Decision

Commit both: Go stubs under `gen/`, Python stubs under `trainer/belady/`.
Pin the generator versions in the Makefiles and `trainer/pyproject.toml`.
`make proto` and `make -C trainer proto` regenerate, `make proto-check` fails on drift, and CI runs it.

Never hand-edit a generated file.

## Consequences

`go build ./...` and `go test ./...` work on a fresh clone with no protoc installed.
A contributor reading the code can see the actual interface a service implements without running a code generator first.

A change to the wire contract shows up as a reviewable diff in the pull request.
This is the main benefit: adding a field to a message is a change to every consumer, and having that visible in review is worth the repository noise.

CI fails on drift, so a proto edited without regenerating is caught rather than discovered later by a service that disagrees about a field number.

The generator version is recorded in the output, which is why the versions are pinned.
An unpinned bump appears as a spurious drift failure with no semantic change in it, and `make proto` warns when the local protoc differs from the pinned version so that failure is diagnosable rather than mysterious.

## Concrete evidence for "regenerate, never edit"

Renaming the module from `github.com/JustinK33/newproj` to `github.com/JustinK33/Belady` looked like a pure text substitution, and on hand-written Go it was.

The generated stubs embed the `go_package` option inside the serialized file descriptor, with a **length prefix**: the bytes read `B5Z3github.com/JustinK33/newproj/...`, where `0x35` and `0x33` are lengths.
The new path is one byte shorter, so the correct bytes are `B4Z2github.com/JustinK33/Belady/...`.

A substitution across the tree updates the string and leaves the prefix stale, producing a descriptor that no longer parses as what it claims to be, with no compile error to say so.
`make proto` produced the correct bytes.

This is the argument in one commit: the file is a build artifact that happens to be committed, and the only safe operation on it is regeneration.

## What was given up

**Generating at build time**, with protoc in the toolchain and `gen/` in `.gitignore`.
The cleaner answer in the abstract, and the one that makes drift impossible by construction rather than by a CI check.
Rejected because it puts protoc, `protoc-gen-go` and `protoc-gen-go-grpc` between a fresh clone and a passing test, and because it hides wire-contract changes from review, which is where they most need to be seen.

**Publishing the stubs as a separate versioned module.**
The right answer for a contract consumed by other repositories.
Rejected as premature for a single-repository project: it adds a release step to every proto change to solve a distribution problem that does not exist yet.
