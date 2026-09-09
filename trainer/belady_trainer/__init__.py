"""Offline trainer for the Belady learned cache policy.

Nothing in here runs on a request path. The pipeline is: read the access traces the
cache nodes sampled, replay them into labelled feature rows, fit a GBDT, and hand the
result to the model registry, which the cache nodes are already watching.
"""

__all__ = ["columns", "dataset", "publish", "samples", "train"]
