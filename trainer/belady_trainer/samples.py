"""Turning an access trace into a labelled training set.

Two things happen here: reconstructing entry state, and choosing when to label.

The features the cache scores a candidate on are derived from state the cache
maintains per entry: when it was admitted, how many times it has been accessed, the
gaps between recent accesses.
None of that is in the trace directly, so it is replayed from the trace, and it has
to be replayed to match `internal/cache/entry.go` exactly.
Admissions are the reset points: a record with `hit=false` is a miss, which means the
object was not resident, so its counters start over.
That is why the trace needs no eviction records.

The obvious thing is to label each access, but the features would then always have
`recency_ms = 0`, because no time has passed.
At eviction the recency is whatever it happens to be, and it is the single most
informative column, so a model trained that way would never have seen the input
distribution it runs on.
Instead each inter-access interval contributes a snapshot at a uniformly random
offset into it, so recency is distributed the way the cache actually observes it.

ponytail: snapshots are taken without simulating residency, so some rows describe
objects a real cache would already have evicted.
That teaches "very idle means evictable", which is correct, and it avoids making the
training set depend on the policy that produced the trace.
Replay against a simulated cache if feature importances start to look policy-shaped.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from . import columns

# The cap in entry.touch: the Go history stores gaps as uint32 milliseconds.
UINT32_MAX = 0xFFFFFFFF

# The ceiling of S3-FIFO's 2-bit saturating counter, which the features expose.
FREQ_MAX = 3


@dataclass(frozen=True)
class Samples:
    x: np.ndarray  # float32, (n, columns.COUNT)
    y: np.ndarray  # uint8, 1 where the next access is beyond the boundary
    timestamp_us: np.ndarray  # int64, when each snapshot was taken
    dropped_censored: int  # rows discarded because the trace ended too soon to label

    def __len__(self) -> int:
        return len(self.y)

    @property
    def positive_rate(self) -> float:
        return float(self.y.mean()) if len(self.y) else 0.0


def build(
    key: np.ndarray,
    timestamp_us: np.ndarray,
    size_bytes: np.ndarray,
    hit: np.ndarray,
    *,
    boundary_us: int,
    max_recency_us: int,
    seed: int = 1,
) -> Samples:
    """Replay the trace and emit one labelled snapshot per inter-access interval."""
    if boundary_us <= 0:
        raise ValueError("boundary_us must be positive")
    if max_recency_us <= 0:
        raise ValueError("max_recency_us must be positive")

    n = len(key)
    if n == 0:
        raise ValueError("the trace is empty")

    # Grouped by key, chronological within each key: lexsort's last key is primary.
    order = np.lexsort((timestamp_us, key))
    k = key[order]
    t = timestamp_us[order].astype(np.int64)
    size = size_bytes[order]
    is_hit = hit[order]
    trace_end = int(timestamp_us.max())

    # A new segment starts at the first record of a key, and at every miss, because a
    # miss means the object was admitted fresh.
    new_key = np.empty(n, dtype=bool)
    new_key[0] = True
    np.not_equal(k[1:], k[:-1], out=new_key[1:])
    new_segment = new_key | ~is_hit

    idx = np.arange(n, dtype=np.int64)
    seg_start = idx[new_segment]
    seg_len = np.diff(np.append(seg_start, n))

    # position within the segment: 0 for the admission, 1 for the first hit after it
    position = idx - np.repeat(seg_start, seg_len)
    admitted = np.repeat(t[seg_start], seg_len)

    accesses = (position + 1).astype(np.int64)
    frequency = np.minimum(accesses, FREQ_MAX)

    # gap_ms[i] is the gap folded into the history by the access at i, matching
    # entry.touch. It is zero at a segment start, where the history is reset.
    gap_us = np.zeros(n, dtype=np.int64)
    np.subtract(t[1:], t[:-1], out=gap_us[1:])
    gap_ms = np.minimum(gap_us // 1000, UINT32_MAX)
    gap_ms[new_segment] = 0

    # time to the next access of the same key, or -1 when there is none in the trace
    next_gap_us = np.full(n, -1, dtype=np.int64)
    same_key_ahead = np.empty(n, dtype=bool)
    same_key_ahead[-1] = False
    np.equal(k[:-1], k[1:], out=same_key_ahead[:-1])
    next_gap_us[:-1] = np.where(same_key_ahead[:-1], t[1:] - t[:-1], -1)

    # Snapshot offset into the interval. Bounded by max_recency_us so a key that
    # vanishes for a day does not contribute a row claiming a day of idleness, which
    # no resident entry would ever have.
    # The offset excludes the next access itself, where the entry is already touched.
    # A key accessed twice inside the same microsecond has a zero-length interval,
    # and the only offset into it is zero.
    rng = np.random.default_rng(seed)
    censored = next_gap_us < 0
    span = np.where(censored, max_recency_us, np.minimum(next_gap_us, max_recency_us))
    delta_us = rng.integers(0, np.maximum(span, 1), dtype=np.int64)

    snapshot_us = t + delta_us
    remaining_us = next_gap_us - delta_us
    beyond = np.where(censored, True, remaining_us > boundary_us)

    # A censored row is only labelled when the trace runs at least a boundary past the
    # snapshot. Otherwise "no next access" may simply mean the recording stopped, and
    # a label that says "never used again" would be an artefact of the trace window.
    keep = ~censored | (snapshot_us + boundary_us <= trace_end)
    dropped = int((~keep).sum())

    x = np.zeros((int(keep.sum()), columns.COUNT), dtype=np.float32)
    age_us = snapshot_us[keep] - admitted[keep]

    x[:, columns.SIZE_BYTES] = size[keep]
    x[:, columns.RECENCY_MS] = delta_us[keep] // 1000
    x[:, columns.AGE_MS] = age_us // 1000
    x[:, columns.ACCESSES] = accesses[keep]
    x[:, columns.FREQUENCY] = frequency[keep]
    # Accesses per second while resident, matching features.Extract. The +1 keeps a
    # freshly admitted object from dividing by nearly zero.
    x[:, columns.REUSE_RATE] = accesses[keep] / (age_us / 1e6 + 1.0)

    kept = np.flatnonzero(keep)
    for j in range(columns.HISTORY_LEN):
        # delta_j is the gap folded in j accesses ago, so it reads j rows back. The
        # position guard is what stops it reading across an admission, or off the front
        # of a trace shorter than the history.
        source = np.zeros(n, dtype=np.int64)
        source[j:] = gap_ms[: max(n - j, 0)]
        x[:, columns.DELTA0 + j] = np.where(position >= j, source, 0)[kept]

    return Samples(
        x=x,
        y=beyond[keep].astype(np.uint8),
        timestamp_us=snapshot_us[keep],
        dropped_censored=dropped,
    )


def reuse_quantiles_us(
    key: np.ndarray,
    timestamp_us: np.ndarray,
    quantiles: list[float],
) -> list[int]:
    """Return the given quantiles of the gaps between accesses to the same key.

    The boundary is a workload property, not a tuning knob: it has to sit inside the
    range of reuse times the cache actually sees.
    Far above them every object is "beyond boundary", far below none are, and either
    way the model learns nothing while still scoring confidently.
    Returns zeros when the trace holds no reuse at all.
    """
    order = np.lexsort((timestamp_us, key))
    k = key[order]
    t = timestamp_us[order].astype(np.int64)
    gaps = (t[1:] - t[:-1])[k[:-1] == k[1:]]
    if len(gaps) == 0:
        return [0] * len(quantiles)
    return [int(v) for v in np.quantile(gaps, quantiles)]
