"""Command line entry point: `python -m belady_trainer ...`.

Configuration is environment-first to match the Go services, and MODEL_BOUNDARY in
particular is read from the same variable the cache nodes read, because a node refuses
a model whose boundary disagrees with its own. Sharing the variable is what makes that
check a safety net rather than a routine failure.
"""

from __future__ import annotations

import argparse
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

_DURATION = re.compile(r"(\d+)(ns|us|ms|s|m|h)")
_UNIT_US = {"ns": 1e-3, "us": 1, "ms": 1e3, "s": 1e6, "m": 60e6, "h": 3600e6}


def parse_duration_us(text: str) -> int:
    """Parse a Go-style duration, e.g. "10m", "1h30m", "600s".

    Go's time.ParseDuration is what the cache nodes use, so the same string has to
    work here. A bare number is read as seconds, which is the one thing Go rejects and
    the one thing people type.
    """
    text = text.strip()
    if not text:
        raise ValueError("empty duration")
    if text.isdigit():
        return int(text) * 1_000_000

    total = 0.0
    consumed = 0
    for match in _DURATION.finditer(text):
        if match.start() != consumed:
            break
        total += int(match.group(1)) * _UNIT_US[match.group(2)]
        consumed = match.end()
    if consumed != len(text) or total <= 0:
        raise ValueError(f"cannot parse duration {text!r}: want e.g. 30s, 10m, 1h30m")
    return int(total)


def _load(directory: str):
    trace = dataset.load(directory)
    print(
        f"loaded {len(trace)} records over {trace.duration_us / 1e6:.1f}s "
        f"from {len(dataset.segments(directory))} segments",
        file=sys.stderr,
    )
    return trace


def cmd_boundary(args: argparse.Namespace) -> int:
    trace = _load(args.traces)
    print("reuse time quantiles (seconds):")
    for q in (0.5, 0.75, 0.9, 0.99):
        us = samples.suggest_boundary_us(trace.key, trace.timestamp_us, quantile=q)
        print(f"  p{int(q * 100):<3} {us / 1e6:12.4f}")
    return 0


def cmd_train(args: argparse.Namespace) -> int:
    boundary_us = parse_duration_us(args.boundary)
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
        median = samples.suggest_boundary_us(trace.key, trace.timestamp_us)
        print(f"error: {err}", file=sys.stderr)
        print(f"hint: the median reuse time in this trace is {median / 1e6:g}s", file=sys.stderr)
        return 2

    print(result.summary())

    body = result.model.model_to_string().encode()
    with open(args.out, "wb") as f:
        f.write(body)
    print(f"wrote {args.out} ({len(body)} bytes)", file=sys.stderr)

    if not args.publish:
        return 0

    meta = publish_mod.publish(
        args.publish,
        body,
        version=publish_mod.version_for(),
        boundary_seconds=boundary_us // 1_000_000,
        feature_count=columns.COUNT,
        metrics={
            "auc": f"{result.auc:.4f}",
            "rows": str(result.rows),
            "positive_rate": f"{result.positive_rate:.4f}",
            "trees": str(result.trees),
        },
    )
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
