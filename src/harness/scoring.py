from __future__ import annotations

from collections.abc import Iterable
from math import comb


def pass_at_k(n: int, c: int, k: int) -> float:
    """Unbiased pass@k estimator used by code-agent benchmarks."""
    if n < 1:
        return 0.0
    if c < 0 or c > n:
        raise ValueError("c must be between 0 and n")
    if k < 1 or k > n:
        raise ValueError("k must be between 1 and n")
    if c == 0:
        return 0.0
    if n - c < k:
        return 1.0
    return 1.0 - comb(n - c, k) / comb(n, k)


def weighted_mean(values: Iterable[tuple[float, float]]) -> float:
    pairs = list(values)
    total_weight = sum(weight for _, weight in pairs)
    if not total_weight:
        return 0.0
    return sum(value * weight for value, weight in pairs) / total_weight
