"""External benchmark adapters and generic benchmark runtime contracts."""

from .base import (
    BenchmarkAdapter,
    BenchmarkCase,
    BenchmarkSession,
    BenchmarkTarget,
    EvaluationResult,
    Submission,
    SubmissionResult,
)
from .runner import BenchmarkRunner

__all__ = [
    "BenchmarkAdapter",
    "BenchmarkCase",
    "BenchmarkRunner",
    "BenchmarkSession",
    "BenchmarkTarget",
    "EvaluationResult",
    "Submission",
    "SubmissionResult",
]
