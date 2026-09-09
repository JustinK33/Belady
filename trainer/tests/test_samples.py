"""Checks on the part of the trainer that has no compiler to catch it.

Everything here is about agreement with Go. The feature columns are filled by
`internal/features/features.go` at serving time and by `samples.py` at training time,
and the entry state those columns derive from is maintained by
`internal/cache/entry.go`. If the replay drifts from the state machine, training still
succeeds and the model still loads; it just scores on inputs that never occur.
"""

from __future__ import annotations

import numpy as np
import pytest

from belady_trainer import columns, samples

SECOND = 1_000_000


def trace(records):
    """Build column arrays from (key, timestamp_us, size, hit) tuples."""
    key, ts, size, hit = zip(*records, strict=True)
    return (
        np.array(key, dtype=np.uint64),
        np.array(ts, dtype=np.int64),
        np.array(size, dtype=np.uint32),
        np.array(hit, dtype=bool),
    )


def deltas(x, row):
    return x[row, columns.DELTA0 : columns.DELTA0 + columns.HISTORY_LEN]


def test_entry_state_is_replayed_from_the_trace():
    key, ts, size, hit = trace(
        [
            (7, 0, 100, False),
            (7, 2 * SECOND, 100, True),
            (7, 5 * SECOND, 100, True),
            (7, 9 * SECOND, 100, True),
        ]
    )
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=4 * SECOND)

    # The final access has no successor, and the trace ends there, so it cannot be
    # labelled. Three intervals remain.
    assert len(got) == 3
    assert got.dropped_censored == 1

    x = got.x
    assert list(x[:, columns.ACCESSES]) == [1, 2, 3]
    # frequency saturates at 3, matching the 2-bit counter in entry.go
    assert list(x[:, columns.FREQUENCY]) == [1, 2, 3]
    assert list(x[:, columns.SIZE_BYTES]) == [100, 100, 100]

    # An admission zeroes the history; each later access shifts one gap in, newest first.
    assert list(deltas(x, 0)) == [0] * 8
    assert list(deltas(x, 1)) == [2000, 0, 0, 0, 0, 0, 0, 0]
    assert list(deltas(x, 2)) == [3000, 2000, 0, 0, 0, 0, 0, 0]


def test_a_miss_resets_the_entry():
    # The middle record is a miss, which means the object was not resident: the cache
    # allocated a fresh entry, so its counters and history start over.
    key, ts, size, hit = trace(
        [
            (7, 0, 64, False),
            (7, 1 * SECOND, 64, True),
            (7, 4 * SECOND, 64, False),
            (7, 6 * SECOND, 64, True),
            (7, 9 * SECOND, 64, True),
        ]
    )
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=4 * SECOND)

    x = got.x
    assert len(got) == 4
    assert list(x[:, columns.ACCESSES]) == [1, 2, 1, 2]
    assert list(deltas(x, 2)) == [0] * 8, "the re-admission kept the old history"
    assert list(deltas(x, 3)) == [2000, 0, 0, 0, 0, 0, 0, 0]

    # age is measured from the latest admission, not from the first sighting.
    assert 2000 <= x[3, columns.AGE_MS] < 5000


def test_the_label_is_the_boundary_test():
    # Two keys, distinguishable by size, with reuse on either side of a 1s boundary.
    # max_recency caps the snapshot offset at 4s, so the 10s gap always leaves more
    # than a boundary of remaining time and the 0.1s gap never does.
    key, ts, size, hit = trace(
        [
            (1, 0, 111, False),
            (1, 10 * SECOND, 111, True),
            (2, 0, 222, False),
            (2, SECOND // 10, 222, True),
        ]
    )
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=4 * SECOND)

    slow = got.y[got.x[:, columns.SIZE_BYTES] == 111]
    fast = got.y[got.x[:, columns.SIZE_BYTES] == 222]

    # Key 1's own last access closes the trace, so its trailing row is unlabellable.
    assert list(slow) == [1], "a 10s gap is beyond a 1s boundary"
    # Key 2 goes quiet at 0.1s and the trace runs to 10s, so it contributes both its
    # observed interval and a labelled trailing row.
    assert list(fast) == [0, 1], "a 0.1s gap is not beyond the boundary, silence after it is"


def test_a_row_the_trace_ends_too_soon_to_label_is_dropped():
    key, ts, size, hit = trace([(7, 0, 8, False), (7, 1000, 8, True)])
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=SECOND)

    # The first interval is fully observed. The second has no next access, and the
    # trace stops immediately, so "never accessed again" cannot be distinguished from
    # "the recording stopped".
    assert len(got) == 1
    assert got.dropped_censored == 1
    assert list(got.y) == [0]


def test_a_final_access_is_labelled_when_the_trace_runs_on():
    # Key 7 goes quiet at t=1ms while key 8 keeps the trace alive until t=5s. With the
    # snapshot offset capped at 1s, key 7's last row is always at least a boundary
    # short of the end, so its silence is real rather than an artefact of the window.
    key, ts, size, hit = trace(
        [
            (7, 0, 8, False),
            (7, 1000, 8, True),
            (8, 0, 16, False),
            (8, 5 * SECOND, 16, True),
        ]
    )
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=SECOND)

    seven = got.y[got.x[:, columns.SIZE_BYTES] == 8]
    assert sorted(seven) == [0, 1], "the final access should be labelled beyond boundary"


def test_recency_is_spread_across_the_interval():
    # The reason snapshots are taken at a random offset rather than at the access
    # itself. Labelling at the access would make recency_ms identically zero in
    # training while it is large and decisive at eviction.
    n = 200
    records = [(7, 0, 32, False)] + [(7, i * 4 * SECOND, 32, True) for i in range(1, n)]
    key, ts, size, hit = trace(records)
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=4 * SECOND)

    recency = got.x[:, columns.RECENCY_MS]
    assert recency.max() > 3000
    assert len(np.unique(recency)) > n // 2
    assert 0.0 < got.positive_rate < 1.0


def test_reuse_rate_matches_the_go_extractor():
    key, ts, size, hit = trace([(7, 0, 100, False), (7, 8 * SECOND, 100, True)])
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=4 * SECOND)

    x = got.x
    age_s = x[:, columns.AGE_MS] / 1000
    expected = x[:, columns.ACCESSES] / (age_s + 1)
    assert np.allclose(x[:, columns.REUSE_RATE], expected, rtol=1e-3)


def test_an_empty_trace_is_an_error():
    empty = (
        np.array([], dtype=np.uint64),
        np.array([], dtype=np.int64),
        np.array([], dtype=np.uint32),
        np.array([], dtype=bool),
    )
    with pytest.raises(ValueError):
        samples.build(*empty, boundary_us=SECOND, max_recency_us=SECOND)


def test_two_accesses_in_the_same_microsecond():
    # The trace clock is microseconds and 64 concurrent clients share hot keys, so
    # zero-length intervals are routine rather than pathological. There is exactly one
    # offset into a zero-length interval, and asking for a random one in (0, 0] fails.
    key, ts, size, hit = trace(
        [
            (7, 0, 40, False),
            (7, 1000, 40, True),
            (7, 1000, 40, True),
            (7, 4 * SECOND, 40, True),
        ]
    )
    got = samples.build(key, ts, size, hit, boundary_us=SECOND, max_recency_us=4 * SECOND)

    assert len(got) == 3
    zero = got.x[:, columns.RECENCY_MS] == 0
    assert zero.any(), "the zero-length interval should be sampled at the access itself"
    assert got.y[zero][0] == 0, "an object accessed again immediately is not beyond the boundary"


def test_suggest_boundary_reports_the_median_gap():
    key, ts, _, _ = trace(
        [(7, 0, 1, False), (7, SECOND, 1, True), (7, 3 * SECOND, 1, True), (7, 6 * SECOND, 1, True)]
    )
    # gaps are 1s, 2s, 3s
    assert samples.suggest_boundary_us(key, ts) == 2 * SECOND
