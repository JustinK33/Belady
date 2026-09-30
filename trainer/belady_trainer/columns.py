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
SIZE_BYTES = NAMES.index("size_bytes")
RECENCY_MS = NAMES.index("recency_ms")
AGE_MS = NAMES.index("age_ms")
ACCESSES = NAMES.index("accesses")
FREQUENCY = NAMES.index("frequency")
REUSE_RATE = NAMES.index("reuse_rate")
DELTA0 = NAMES.index("delta_0")
