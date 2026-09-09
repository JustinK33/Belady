"""Reading the segment format the Go recorder writes.

The framing is hand-rolled on both sides, so it is worth a test that a Python reader
sees exactly what a Go writer produced, including the case that actually happens in
production: the node was killed part-way through a frame.
"""

from __future__ import annotations

import pytest

from belady.v1 import trace_pb2
from belady_trainer import dataset


def varint(n: int) -> bytes:
    out = bytearray()
    while n >= 0x80:
        out.append((n & 0x7F) | 0x80)
        n >>= 7
    out.append(n)
    return bytes(out)


def batch(node: str, records) -> bytes:
    msg = trace_pb2.AccessBatch(node_id=node)
    for key, ts, size, hit in records:
        msg.records.add(key_hash=key, timestamp_us=ts, size_bytes=size, hit=hit)
    body = msg.SerializeToString()
    return varint(len(body)) + body


def write(directory, name: str, payload: bytes) -> None:
    (directory / name).write_bytes(payload)


def test_round_trip_across_segments(tmp_path):
    write(tmp_path, "0000000001.trace", batch("a", [(1, 100, 64, False), (2, 200, 128, True)]))
    # Out of order on purpose: shards and nodes interleave, and load has to sort.
    write(tmp_path, "0000000002.trace", batch("b", [(3, 50, 32, True)]))

    got = dataset.load(str(tmp_path))
    assert list(got.key) == [3, 1, 2]
    assert list(got.timestamp_us) == [50, 100, 200]
    assert list(got.size_bytes) == [32, 64, 128]
    assert list(got.hit) == [True, False, True]
    assert got.duration_us == 150


def test_an_unfinished_segment_is_skipped(tmp_path):
    write(tmp_path, "0000000001.trace", batch("a", [(1, 100, 64, False)]))
    # The recorder writes to .partial and renames on close, so a .partial file is a
    # segment the cache is still appending to.
    write(tmp_path, "0000000002.partial", batch("a", [(9, 900, 64, False)]))

    got = dataset.load(str(tmp_path))
    assert list(got.key) == [1]


def test_a_truncated_tail_is_dropped_not_fatal(tmp_path):
    good = batch("a", [(1, 100, 64, False)])
    torn = batch("a", [(2, 200, 64, True)])
    write(tmp_path, "0000000001.trace", good + torn[: len(torn) // 2])

    got = dataset.load(str(tmp_path))
    assert list(got.key) == [1], "records before the torn frame should survive"


def test_an_empty_directory_is_an_error(tmp_path):
    with pytest.raises(FileNotFoundError):
        dataset.load(str(tmp_path))
