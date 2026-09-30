"""Command line entry point: `python -m belady_trainer ...`.

Configuration is environment-first to match the Go services. MODEL_BOUNDARY is read
from the same variable the cache nodes read, because a node refuses a model whose
boundary disagrees with its own.
"""

from __future__ import annotations

import argparse
import math
import os
import re
import sys

from . import columns, dataset, samples
from . import publish as publish_mod
from . import train as train_mod

DEFAULT_BOUNDARY = "10m"

# How much idleness a training row may claim, as a multiple of the boundary. Past a few
# boundaries every row is labelled the same way, so the extra rows carry no signal and
# only skew the recency distribution away from what a resident entry looks like.
MAX_RECENCY_BOUNDARIES = 4

_UNIT_NS = {
    "ns": 1,
    "us": 1_000,
    "\u00b5s": 1_000,  # micro sign
    "\u03bcs": 1_000,  # Greek mu
    "ms": 1_000_000,
    "s": 1_000_000_000,
    "m": 60_000_000_000,
    "h": 3_600_000_000_000,
}
_COMPONENT = re.compile(r"(\d*)(?:\.(\d*))?([^\d.]*)")
_MAX_NS = 1 << 63


def parse_duration_ns(text: str) -> int:
    """Parse a duration exactly as Go's time.ParseDuration does, e.g. "1.5m", "1h30m".

    The cache nodes parse MODEL_BOUNDARY with Go, so a string has to mean the same
    thing here or be refused here too. That includes refusing a bare number such as
    "600", which Go rejects.
    """
    s = text
    neg = s[:1] == "-"
    if s[:1] in ("-", "+"):
        s = s[1:]
    if s == "0":
        return 0
    if not s:
        raise ValueError(f"invalid duration {text!r}")

    total = 0
    pos = 0
    while pos < len(s):
        m = _COMPONENT.match(s, pos)
        whole, frac, unit = m.group(1), m.group(2), m.group(3)
        if not whole and not frac:
            raise ValueError(f"invalid duration {text!r}")
        if unit not in _UNIT_NS:
            raise ValueError(f"unknown unit {unit!r} in duration {text!r}")
        scale = _UNIT_NS[unit]

        v = int(whole or 0)
        if v > _MAX_NS // scale:
            raise ValueError(f"invalid duration {text!r}")
        v *= scale
        if frac:
            # Go keeps fraction digits only while they fit in an int64, then applies
            # them in float64, so this does the same to round identically.
            f, fscale = 0, 1.0
            for digit in frac:
                if f > (_MAX_NS - 1) // 10 or f * 10 + int(digit) >= _MAX_NS:
                    break
                f = f * 10 + int(digit)
                fscale *= 10
            v += int(float(f) * (scale / fscale))
        total += v
        if total > _MAX_NS:
            raise ValueError(f"invalid duration {text!r}")
        pos = m.end()

    if neg:
        return -total
    if total == _MAX_NS:
        raise ValueError(f"invalid duration {text!r}")
    return total


def _load(directory: str):
    try:
        trace = dataset.load(directory)
    except (OSError, ValueError) as err:
        print(f"error: {err}", file=sys.stderr)
        raise SystemExit(1) from err
    print(
        f"loaded {len(trace)} records over {trace.duration_us / 1e6:.1f}s "
        f"from {len(dataset.segments(directory))} segments",
        file=sys.stderr,
    )
    return trace


def cmd_boundary(args: argparse.Namespace) -> int:
    trace = _load(args.traces)
    print("reuse time quantiles (seconds):")
    qs = [0.5, 0.75, 0.9, 0.99]
    for q, us in zip(
        qs, samples.reuse_quantiles_us(trace.key, trace.timestamp_us, qs), strict=True
    ):
        print(f"  p{int(q * 100):<3} {us / 1e6:12.4f}")
    return 0


def whole_seconds(boundary_us: int) -> int:
    """Convert a boundary to the whole seconds ModelMeta carries, or refuse.

    ModelMeta.boundary_seconds is a uint32 of seconds, and a cache node compares it
    against its own MODEL_BOUNDARY exactly. Rounding would publish a model fit at one
    boundary under the label of another: 1500ms truncates to 1, so a node set to 1s
    would accept labels that mean something else.
    """
    if boundary_us % 1_000_000 != 0 or not 1_000_000 <= boundary_us <= 0xFFFFFFFF * 1_000_000:
        raise ValueError(
            f"{boundary_us / 1e6:g}s is not a boundary the registry metadata can "
            f"carry. ModelMeta.boundary_seconds is whole seconds, so the boundary "
            f"must be a whole number of seconds and at least 1s. Run `boundary` for "
            f"this trace's reuse times and pick between p90 and p99."
        )
    return boundary_us // 1_000_000


def cmd_train(args: argparse.Namespace) -> int:
    # Checked before _load, so a bad boundary fails before the fit rather than after.
    try:
        boundary_us = parse_duration_ns(args.boundary) // 1000
        boundary_seconds = whole_seconds(boundary_us)
    except ValueError as err:
        print(f"error: MODEL_BOUNDARY={args.boundary!r}: {err}", file=sys.stderr)
        return 2

    trace = _load(args.traces)

    data = samples.build(
        trace.key,
        trace.timestamp_us,
        trace.size_bytes,
        trace.hit,
        boundary_us=boundary_us,
        max_recency_us=boundary_us * MAX_RECENCY_BOUNDARIES,
        seed=args.seed,
    )
    print(f"built {len(data)} rows, {data.dropped_censored} dropped as censored", file=sys.stderr)

    try:
        result = train_mod.fit(data, boundary_us=boundary_us, seed=args.seed)
    except train_mod.DegenerateLabels as err:
        # p90 rather than the median: the label is "reused beyond the boundary", so a
        # boundary at the median leaves half the rows positive on a slow workload and,
        # on a fast one, rounds to the 1s floor anyway. The usable band is p90 to p99.
        p90, p99 = samples.reuse_quantiles_us(trace.key, trace.timestamp_us, [0.9, 0.99])
        lo, hi = max(1, math.ceil(p90 / 1e6)), math.floor(p99 / 1e6)
        print(f"error: {err}", file=sys.stderr)
        if lo <= hi:
            hint = f"try MODEL_BOUNDARY between {lo}s and {hi}s, the p90 to p99 reuse times."
        else:
            hint = (
                f"p99 reuse time is {p99 / 1e6:g}s, below the 1s minimum boundary. "
                f"Capture a longer or slower trace."
            )
        print(
            f"hint: {hint} Run `boundary` for the full distribution.",
            file=sys.stderr,
        )
        return 2

    print(result.summary())

    body = result.model.model_to_string().encode()
    with open(args.out, "wb") as f:
        f.write(body)
    print(f"wrote {args.out} ({len(body)} bytes)", file=sys.stderr)

    if not args.publish:
        return 0

    try:
        meta = publish_mod.publish(
            args.publish,
            body,
            version=publish_mod.version_for(),
            boundary_seconds=boundary_seconds,
            feature_count=columns.COUNT,
            metrics={
                "auc": f"{result.auc:.4f}",
                "rows": str(result.rows),
                "positive_rate": f"{result.positive_rate:.4f}",
                "trees": str(result.trees),
            },
        )
    except publish_mod.PublishError as err:
        print(f"error: publish failed: {err}", file=sys.stderr)
        return 1
    print(f"published {meta.version} to {args.publish}", file=sys.stderr)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="belady_trainer", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    t = sub.add_parser("train", help="fit a model from captured traces")
    t.add_argument("--traces", default=os.getenv("TRACE_DIR", "traces"))
    t.add_argument("--boundary", default=os.getenv("MODEL_BOUNDARY", DEFAULT_BOUNDARY))
    t.add_argument("--out", default=os.getenv("MODEL_OUT", "model.txt"))
    t.add_argument(
        "--publish",
        default=os.getenv("REGISTRY_ADDR", ""),
        help="registry address, or empty to skip publishing",
    )
    t.add_argument("--seed", type=int, default=1)
    t.set_defaults(func=cmd_train)

    b = sub.add_parser("boundary", help="report the reuse times in a trace")
    b.add_argument("--traces", default=os.getenv("TRACE_DIR", "traces"))
    b.set_defaults(func=cmd_boundary)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
