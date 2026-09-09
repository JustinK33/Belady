"""The feature layout, mirrored from internal/features/features.go.

This module and that file are the same contract written twice, because the two
sides run in different languages and neither can import the other.
A mismatch is silent: the model still loads, still evaluates, and reads the wrong
column for every split.
So `TestNamesMatchTheTrainer` in internal/features parses this file and fails the
Go build if the two ever drift.
"""

HISTORY_LEN = 8

# Order matters. It is the order the Go extractor writes and the order LightGBM
# will bake into the model's split_feature indices.
NAMES = [
    "size_bytes",
    "recency_ms",
    "age_ms",
    "accesses",
    "frequency",
    "reuse_rate",
    *[f"delta_{i}" for i in range(HISTORY_LEN)],
]

COUNT = len(NAMES)

# Indices, for the code that fills the matrix.
SIZE_BYTES = 0
RECENCY_MS = 1
AGE_MS = 2
ACCESSES = 3
FREQUENCY = 4
REUSE_RATE = 5
DELTA0 = 6
