"""Fitting the eviction model.

The task is binary: given the features of a resident object, will its next access
fall beyond the Belady boundary?
At eviction the cache samples a handful of candidates, scores them, and evicts the
highest score, so only the *ranking* of a few objects matters and the absolute
probability never does.
That is why a shallow GBDT is enough, and why AUC is the metric worth reading.

The LightGBM parameters here are not stylistic.
`internal/model` is a hand-written parser for LightGBM's text dump that supports
exactly one node shape: a numeric threshold with a two-way branch.
Anything else - categorical splits, a missing-value branch, linear leaves, multiclass
- makes the model unloadable, so the parameters that would produce them are pinned
off here rather than discovered at rollout time.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

import lightgbm as lgb
import numpy as np

from . import columns, samples

# Splitting the holdout by time, not at random. Rows from the same object's history
# are highly correlated, so a random split would put near-duplicates on both sides and
# report an AUC the model cannot reproduce on tomorrow's traffic.
HOLDOUT_FRACTION = 0.2

# Below this the labels are too one-sided for the model to learn a ranking: it can hit
# a high accuracy by answering the majority class every time, and the scores it
# produces carry no order. The boundary is what needs fixing, not the model.
MIN_CLASS_RATE = 0.02

PARAMS: dict[str, object] = {
    "objective": "binary",
    "metric": ["auc", "binary_logloss"],
    "learning_rate": 0.1,
    "num_leaves": 32,
    # Depth is the inference budget. Eviction scores CACHE_SAMPLE_SIZE candidates
    # inside the request path, so each tree is a handful of dependent loads and the
    # total has to stay under a microsecond. See docs/03-performance.md.
    "max_depth": 8,
    "min_data_in_leaf": 50,
    "feature_fraction": 0.9,
    "bagging_fraction": 0.8,
    "bagging_freq": 1,
    "verbosity": -1,
    # The three that keep the dump parseable by internal/model.
    "use_missing": False,
    "zero_as_missing": False,
    "linear_tree": False,
}

MAX_ROUNDS = 300
EARLY_STOPPING_ROUNDS = 25


class DegenerateLabels(Exception):
    """Raised when the boundary produces labels a model cannot learn from."""


@dataclass
class Result:
    model: lgb.Booster
    boundary_us: int
    rows: int
    positive_rate: float
    auc: float
    trees: int
    dropped_censored: int
    importance: dict[str, int] = field(default_factory=dict)

    def summary(self) -> str:
        top = sorted(self.importance.items(), key=lambda kv: -kv[1])[:5]
        return json.dumps(
            {
                "boundary_seconds": self.boundary_us / 1e6,
                "rows": self.rows,
                "positive_rate": round(self.positive_rate, 4),
                "holdout_auc": round(self.auc, 4),
                "trees": self.trees,
                "dropped_censored": self.dropped_censored,
                "top_features": [name for name, _ in top],
            },
            indent=2,
        )


def fit(data: samples.Samples, *, boundary_us: int, seed: int = 1) -> Result:
    """Train on the older rows and score on the newer ones."""
    rate = data.positive_rate
    if rate < MIN_CLASS_RATE or rate > 1 - MIN_CLASS_RATE:
        raise DegenerateLabels(
            f"{rate:.4%} of rows are labelled 'beyond boundary', which is too one-sided "
            f"to rank candidates with. The {boundary_us / 1e6:g}s boundary sits outside "
            "the reuse times in this trace; the hint below suggests one that does not."
        )

    order = np.argsort(data.timestamp_us, kind="stable")
    split = int(len(order) * (1 - HOLDOUT_FRACTION))
    train_idx, valid_idx = order[:split], order[split:]

    train = lgb.Dataset(
        data.x[train_idx],
        label=data.y[train_idx],
        feature_name=list(columns.NAMES),
        params=PARAMS,
    )
    valid = train.create_valid(data.x[valid_idx], label=data.y[valid_idx])

    booster = lgb.train(
        {**PARAMS, "seed": seed},
        train,
        num_boost_round=MAX_ROUNDS,
        valid_sets=[valid],
        callbacks=[lgb.early_stopping(EARLY_STOPPING_ROUNDS, verbose=False)],
    )

    names = booster.feature_name()
    if names != list(columns.NAMES):
        raise RuntimeError(f"LightGBM reordered the features to {names}")

    gains = booster.feature_importance(importance_type="gain")
    return Result(
        model=booster,
        boundary_us=boundary_us,
        rows=len(data),
        positive_rate=rate,
        auc=float(booster.best_score["valid_0"]["auc"]),
        trees=booster.num_trees(),
        dropped_censored=data.dropped_censored,
        importance=dict(zip(names, (int(g) for g in gains), strict=True)),
    )
