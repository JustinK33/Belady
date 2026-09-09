"""Reading the access-trace segments a cache node writes.

The wire format is a sequence of length-delimited `AccessBatch` messages: a
protobuf varint byte count followed by that many bytes.
Protobuf is not self-delimiting, so a file of concatenated messages cannot be split
back apart without the prefix.
The Go writer is internal/trace/recorder.go and the Go reader is
internal/trace/reader.go; this is the third implementation of the same twelve lines
and deliberately so, because the alternative is the trainer depending on a Go
binary.
"""

from __future__ import annotations

import glob
import os
from dataclasses import dataclass

import numpy as np

from belady.v1 import trace_pb2

EXTENSION = ".trace"

# The suffix a cache node writes under before it publishes. Only used to explain an
# empty directory, which is almost always a segment that has not rotated yet.
PARTIAL_EXTENSION = ".partial"


@dataclass(frozen=True)
class Trace:
    """One access stream as columns rather than objects.

    Struct-of-arrays because every step after this is vectorised, and a few million
    Python objects would dominate the runtime of a job whose actual work is a
    LightGBM fit.
    """

    key: np.ndarray  # uint64, the 64-bit key hash
    timestamp_us: np.ndarray  # int64
    size_bytes: np.ndarray  # uint32
    hit: np.ndarray  # bool

    def __len__(self) -> int:
        return len(self.key)

    @property
    def duration_us(self) -> int:
        if len(self) == 0:
            return 0
        return int(self.timestamp_us[-1] - self.timestamp_us[0])


def _read_varint(buf: memoryview, pos: int) -> tuple[int, int]:
    """Decode a base-128 varint, returning the value and the new position."""
    result = 0
    shift = 0
    while True:
        if pos >= len(buf):
            raise ValueError("truncated varint")
        byte = buf[pos]
        pos += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, pos
        shift += 7
        if shift > 63:
            raise ValueError("varint is too long to be a length prefix")


def read_batches(path: str):
    """Yield every AccessBatch in one segment file.

    A truncated tail means the writer died mid-frame.
    Everything before it is still valid, so it is yielded and the tail is dropped
    with a warning rather than failing the whole training run.
    """
    name = os.path.basename(path)
    with open(path, "rb") as f:
        data = memoryview(f.read())

    pos = 0
    while pos < len(data):
        try:
            size, pos = _read_varint(data, pos)
        except ValueError:
            print(f"warning: {name}: corrupt length prefix, dropping the tail")
            return
        if pos + size > len(data):
            print(f"warning: {name}: frame claims {size} bytes, dropping the tail")
            return
        batch = trace_pb2.AccessBatch()
        batch.ParseFromString(bytes(data[pos : pos + size]))
        pos += size
        yield batch


def segments(directory: str) -> list[str]:
    """List finished segments, oldest first.

    Names carry a nanosecond timestamp, so lexical order is chronological order.
    Unfinished segments carry a different suffix and are skipped, which is what makes
    it safe to train from a directory the cache is still writing to.
    """
    return sorted(glob.glob(os.path.join(directory, "*" + EXTENSION)))


def load(directory: str) -> Trace:
    """Load every segment in a directory into one time-ordered trace.

    Records arrive interleaved from many shards and many nodes, so they are sorted by
    timestamp at the end.
    The sort is stable, which keeps two records that share a microsecond in the order
    the cache produced them.
    """
    paths = segments(directory)
    if not paths:
        open_segments = glob.glob(os.path.join(directory, "*" + PARTIAL_EXTENSION))
        if open_segments:
            raise FileNotFoundError(
                f"no {EXTENSION} segments in {directory}, but {len(open_segments)} "
                f"{PARTIAL_EXTENSION} file(s) are still open: a segment publishes when "
                "it reaches TRACE_SEGMENT_BYTES, when TRACE_SEGMENT_MAX_AGE elapses, or "
                "when the node shuts down cleanly"
            )
        raise FileNotFoundError(f"no {EXTENSION} segments in {directory}")

    keys: list[np.ndarray] = []
    times: list[np.ndarray] = []
    sizes: list[np.ndarray] = []
    hits: list[np.ndarray] = []

    for path in paths:
        for batch in read_batches(path):
            records = batch.records
            if not records:
                continue
            n = len(records)
            keys.append(np.fromiter((r.key_hash for r in records), dtype=np.uint64, count=n))
            times.append(np.fromiter((r.timestamp_us for r in records), dtype=np.int64, count=n))
            sizes.append(np.fromiter((r.size_bytes for r in records), dtype=np.uint32, count=n))
            hits.append(np.fromiter((r.hit for r in records), dtype=bool, count=n))

    if not keys:
        raise ValueError(f"{len(paths)} segments in {directory} hold no records")

    key = np.concatenate(keys)
    timestamp_us = np.concatenate(times)
    size_bytes = np.concatenate(sizes)
    hit = np.concatenate(hits)

    order = np.argsort(timestamp_us, kind="stable")
    return Trace(
        key=key[order],
        timestamp_us=timestamp_us[order],
        size_bytes=size_bytes[order],
        hit=hit[order],
    )
