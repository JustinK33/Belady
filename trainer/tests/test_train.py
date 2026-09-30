"""Fitting, and the constraints the Go evaluator imposes on the fit."""

from __future__ import annotations

import re

import numpy as np
import pytest

from belady_trainer import columns, samples, train
from belady_trainer.__main__ import parse_duration_ns, whole_seconds

SECOND = 1_000_000
BOUNDARY = SECOND


def synthetic():
    """A trace with two populations: 10 keys reused every 0.5s, 80 every 4s.

    Reuse straddles a 1s boundary, so the labels are learnable from the delta history
    and roughly balanced, which is the regime the real workload is tuned into.

    Both populations share one object size. Tagging them by size would let the model
    read the answer off a column that carries no such information in a real workload,
    and the AUC assertion below would then prove nothing.
    """
    records = []
    for k in range(10):
        for i in range(120):
            records.append((k, i * SECOND // 2, 1024, i > 0))
    for k in range(100, 180):
        for i in range(15):
            records.append((k, i * 4 * SECOND, 1024, i > 0))

    key, ts, size, hit = zip(*records, strict=True)
    return samples.build(
        np.array(key, dtype=np.uint64),
        np.array(ts, dtype=np.int64),
        np.array(size, dtype=np.uint32),
        np.array(hit, dtype=bool),
        boundary_us=BOUNDARY,
        max_recency_us=4 * BOUNDARY,
    )


def test_the_model_learns_the_boundary():
    result = train.fit(synthetic(), boundary_us=BOUNDARY)

    assert 0.1 < result.positive_rate < 0.9
    assert result.auc > 0.9, f"holdout AUC {result.auc:.3f} on a separable workload"
    assert result.trees > 0
    # Snapshots are offset into the interval rather than taken at the access so that
    # recency carries signal. If it is not near the top of the gain ranking, the sampling
    # has gone wrong and the model is leaning on something it will not see at eviction.
    ranked = sorted(result.importance, key=result.importance.get, reverse=True)
    assert "recency_ms" in ranked[:3], result.importance


def test_the_dump_only_uses_nodes_the_go_evaluator_understands():
    # internal/model implements one node shape: numeric threshold, two-way branch. A
    # categorical split (bit 0) or a non-default missing mode (bits 2-3) makes the
    # model unloadable, and the failure would surface at rollout, not here.
    dump = train.fit(synthetic(), boundary_us=BOUNDARY).model.model_to_string()

    types = {
        int(v)
        for line in dump.splitlines()
        if line.startswith("decision_type=")
        for v in line.removeprefix("decision_type=").split()
    }
    assert types <= {0, 2}, f"unsupported decision types in the dump: {sorted(types)}"
    assert "is_linear=1" not in dump
    assert re.search(r"^num_class=1$", dump, re.M)

    names = re.search(r"^feature_names=(.*)$", dump, re.M).group(1).split()
    assert names == list(columns.NAMES)


def test_a_boundary_outside_the_workload_is_refused():
    # An hour-long boundary against a minute-long trace labels everything "will be
    # reused". A model fitted on that scores confidently and ranks nothing.
    data = synthetic()
    hour = 3600 * SECOND
    flat = samples.Samples(
        x=data.x, y=np.zeros_like(data.y), timestamp_us=data.timestamp_us, dropped_censored=0
    )
    with pytest.raises(train.DegenerateLabels, match="one-sided"):
        train.fit(flat, boundary_us=hour)


# Expected values come from Go's time.ParseDuration.
@pytest.mark.parametrize(
    ("text", "want_ns"),
    [
        ("0", 0),
        ("+0", 0),
        ("-0", 0),
        ("600s", 600_000_000_000),
        ("10m", 600_000_000_000),
        ("1.5m", 90_000_000_000),
        ("1h30m", 5_400_000_000_000),
        ("2h45m30.5s", 9_930_500_000_000),
        (".5s", 500_000_000),
        ("1.s", 1_000_000_000),
        ("250ms", 250_000_000),
        ("1us", 1000),
        ("1\u00b5s", 1000),
        ("1\u03bcs", 1000),
        ("1500ns", 1500),
        ("+2m", 120_000_000_000),
        ("-1.5h", -5_400_000_000_000),
        ("0.000001s", 1000),
        ("1.0000000000000000001s", 1_000_000_000),
        ("3.33333333333333333333m", 200_000_000_000),
        ("9223372036854775807ns", 9_223_372_036_854_775_807),
    ],
)
def test_durations_parse_as_go_parses_them(text, want_ns):
    assert parse_duration_ns(text) == want_ns


@pytest.mark.parametrize(
    "text",
    [
        "600",
        "",
        " ",
        "1",
        "s",
        ".s",
        "1x",
        "1.5",
        "1 m",
        "--1s",
        "1e3s",
        "10m ",
        "m10",
        "1h30",
        "9223372036854775808ns",
        "2562048h",
        "1.2.3s",
    ],
)
def test_durations_go_refuses_are_refused(text):
    with pytest.raises(ValueError):
        parse_duration_ns(text)


@pytest.mark.parametrize(
    ("boundary_us", "want_seconds"),
    [(SECOND, 1), (30 * SECOND, 30), (600 * SECOND, 600)],
)
def test_a_whole_second_boundary_survives_the_metadata(boundary_us, want_seconds):
    assert whole_seconds(boundary_us) == want_seconds


@pytest.mark.parametrize("boundary_us", [500_000, 1_500_000, 999_999, 0, 60_000_001])
def test_a_boundary_the_metadata_cannot_carry_is_refused(boundary_us):
    """ModelMeta.boundary_seconds is whole seconds, and a node compares it exactly.

    Rounding instead of refusing publishes a model fit at one boundary under the label
    of another, so a node set to the real value refuses it while a node set to the
    rounded one accepts labels that mean something else.
    """
    with pytest.raises(ValueError, match="whole number of seconds"):
        whole_seconds(boundary_us)


def test_a_holdout_with_one_class_is_refused():
    # The newest fifth of rows is all positive, so the holdout has no negatives and
    # its AUC would be undefined.
    data = synthetic()
    order = np.argsort(data.timestamp_us, kind="stable")
    y = np.zeros_like(data.y)
    y[order[int(len(y) * 0.8) :]] = 1
    drifted = samples.Samples(x=data.x, y=y, timestamp_us=data.timestamp_us, dropped_censored=0)
    with pytest.raises(train.DegenerateLabels, match="single class"):
        train.fit(drifted, boundary_us=BOUNDARY)


def test_no_rows_is_refused():
    empty = samples.Samples(
        x=np.zeros((0, columns.COUNT), dtype=np.float32),
        y=np.zeros(0, dtype=np.uint8),
        timestamp_us=np.zeros(0, dtype=np.int64),
        dropped_censored=5,
    )
    with pytest.raises(train.DegenerateLabels, match="no rows"):
        train.fit(empty, boundary_us=BOUNDARY)
