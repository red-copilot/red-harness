from __future__ import annotations

from enum import StrEnum


class FailureClass(StrEnum):
    MODEL_TIMEOUT = "model_timeout"
    MODEL_RATE_LIMIT = "model_rate_limit"
    MODEL_PROVIDER_ERROR = "model_provider_error"
    TOOL_TIMEOUT = "tool_timeout"
    TOOL_ERROR = "tool_error"
    INVALID_TOOL_INPUT = "invalid_tool_input"
    MALFORMED_TOOL_RESULT = "malformed_tool_result"
    POLICY_DENIED = "policy_denied"
    INVALID_TARGET = "invalid_target"
    AUTH_FAILURE = "auth_failure"
    BENCHMARK_TRANSIENT_ERROR = "benchmark_transient_error"
    BENCHMARK_REJECTED = "benchmark_rejected"
    NO_PROGRESS = "no_progress"
    CONTRADICTED_HYPOTHESIS = "contradicted_hypothesis"
    STALE_STATE = "stale_state"
    BUDGET_EXHAUSTED = "budget_exhausted"
    OBJECTIVE_IMPOSSIBLE = "objective_impossible"
    INTERNAL_ERROR = "internal_error"


def classify_failure(
    *,
    timed_out: bool = False,
    budget_exceeded: str | None = None,
    returncode: int | None = None,
) -> FailureClass | None:
    if timed_out:
        return FailureClass.TOOL_TIMEOUT
    if budget_exceeded is not None:
        return FailureClass.BUDGET_EXHAUSTED
    if returncode not in {None, 0}:
        return FailureClass.TOOL_ERROR
    return None
