"""Lifecycle status policy shared by local and external benchmark runs."""
from __future__ import annotations

from typing import Literal

RunStatus = Literal["objective_completed", "timeout", "budget_exceeded", "finished"]


def resolve_run_status(
    *,
    timed_out: bool,
    budget_exceeded: bool,
    objective_completed: bool = False,
) -> RunStatus:
    """The externally evaluated objective takes precedence over agent limits.

    The local runner does not pass objective_completed until verification is
    performed; benchmark runs can finish early when a submission is accepted.
    """
    if objective_completed:
        return "objective_completed"
    if timed_out:
        return "timeout"
    if budget_exceeded:
        return "budget_exceeded"
    return "finished"
