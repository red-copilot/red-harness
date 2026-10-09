"""Pure, bounded decisions for the interactive solver loop."""

from __future__ import annotations

import math
from collections.abc import Iterable
from dataclasses import dataclass
from enum import StrEnum


class SolverAction(StrEnum):
    CONTINUE = "continue"
    VERIFY = "verify"
    REPLAN = "replan"
    STOP = "stop"


@dataclass(frozen=True)
class SolverDecision:
    action: SolverAction
    reason: str


VERIFY_EVENTS = frozenset({"tool.result", "progress.updated", "world.observe"})
DEFAULT_MAX_NO_PROGRESS = 6
DEFAULT_MAX_REPLANS = 8
NO_PROGRESS_REPLAN_THRESHOLD = 2
LOW_BUDGET_FRACTION = 0.05


def decide_solver_action(
    *,
    event_type: str | None = None,
    replan_reasons: Iterable[str] = (),
    no_progress_count: int = 0,
    replans_used: int = 0,
    remaining_budget_fraction: float = 1.0,
    force_replan: bool = False,
    max_no_progress: int = DEFAULT_MAX_NO_PROGRESS,
    max_replans: int = DEFAULT_MAX_REPLANS,
) -> SolverDecision:
    """Choose one next action using persisted progress and hard safety bounds."""
    if no_progress_count < 0 or replans_used < 0:
        raise ValueError("solver counters must be non-negative")
    if max_no_progress < 1 or max_replans < 1:
        raise ValueError("solver bounds must be positive")
    if not math.isfinite(remaining_budget_fraction):
        raise ValueError("remaining budget fraction must be finite")

    reasons = tuple(dict.fromkeys(reason for reason in replan_reasons if reason))
    if no_progress_count >= max_no_progress:
        return SolverDecision(SolverAction.STOP, "no_progress_limit")
    if remaining_budget_fraction <= 0:
        return SolverDecision(SolverAction.STOP, "budget_exhausted")
    if remaining_budget_fraction <= LOW_BUDGET_FRACTION and no_progress_count >= NO_PROGRESS_REPLAN_THRESHOLD:
        return SolverDecision(SolverAction.STOP, "low_budget_without_progress")

    should_replan = force_replan or bool(reasons) or no_progress_count >= NO_PROGRESS_REPLAN_THRESHOLD
    if should_replan:
        if replans_used >= max_replans:
            return SolverDecision(SolverAction.STOP, "replan_limit")
        reason = "forced_replan" if force_replan else (reasons[0] if reasons else "no_progress_threshold")
        return SolverDecision(SolverAction.REPLAN, reason)
    if event_type in VERIFY_EVENTS:
        return SolverDecision(SolverAction.VERIFY, "verification_event")
    return SolverDecision(SolverAction.CONTINUE, "no_control_action_required")
