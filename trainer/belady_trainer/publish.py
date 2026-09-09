"""Pushing a trained model to the registry.

The registry re-validates everything sent here: it recomputes the digest, checks the
declared size against the bytes it received, and refuses a version that is not a bare
filename.
None of that is a reason to skip validating on this side.
A model rejected after 40 MiB have crossed the wire is a worse failure than one
rejected before the stream opens, and the digest is the only thing that distinguishes
a truncated upload from a short model.
"""

from __future__ import annotations

import hashlib
import time
from collections.abc import Iterator

import grpc

from belady.v1 import registry_pb2, registry_pb2_grpc

FORMAT = "lightgbm-text"

# Matched to the registry's own chunk size. Large enough that a multi-megabyte dump is
# a few dozen messages, small enough to stay well under the 4 MiB default gRPC limit.
CHUNK_BYTES = 256 * 1024


def version_for(when: float | None = None) -> str:
    """A sortable version string the registry will accept.

    No dots and no separators: the registry stores models as files named after the
    version and rejects anything that could escape its directory.
    """
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime(when))


def _requests(
    meta: registry_pb2.ModelMeta, body: bytes
) -> Iterator[registry_pb2.PublishModelRequest]:
    yield registry_pb2.PublishModelRequest(meta=meta)
    for off in range(0, len(body), CHUNK_BYTES):
        yield registry_pb2.PublishModelRequest(chunk=body[off : off + CHUNK_BYTES])


def publish(
    address: str,
    body: bytes,
    *,
    version: str,
    boundary_seconds: int,
    feature_count: int,
    metrics: dict[str, str] | None = None,
    timeout: float = 60.0,
) -> registry_pb2.ModelMeta:
    """Stream one model to the registry and return the metadata it accepted."""
    if not body:
        raise ValueError("refusing to publish an empty model")
    if boundary_seconds <= 0:
        raise ValueError("boundary_seconds must be positive")

    meta = registry_pb2.ModelMeta(
        version=version,
        format=FORMAT,
        size_bytes=len(body),
        sha256=hashlib.sha256(body).hexdigest(),
        feature_count=feature_count,
        boundary_seconds=boundary_seconds,
        metrics=metrics or {},
    )

    # ponytail: insecure channel, matching the dev-mode gRPC fallback the Go services
    # use. Swap in grpc.ssl_channel_credentials when mTLS lands; see docs/06-security.md.
    with grpc.insecure_channel(address) as channel:
        client = registry_pb2_grpc.RegistryStub(channel)
        return client.PublishModel(_requests(meta, body), timeout=timeout).meta
