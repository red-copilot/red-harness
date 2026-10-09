"""Lifecycle status policy shared by local and external benchmark runs."""

from __future__ import annotations

from typing import Literal

RunStatus = Literal["objective_completed", "timeout", "budget_exceeded", "finished"]
RunPhase = Literal[
    "created",
    "running",
    "paused",
    "recovering",
    "verifying",
    "succeeded",
    "failed",
    "exhausted",
    "cancelled",
]

_TERMINAL_PHASES = frozenset({"succeeded", "failed", "exhausted", "cancelled"})
_TRANSITIONS: dict[RunPhase, frozenset[RunPhase]] = {
    "created": frozenset({"running", "cancelled", "failed"}),
    "running": frozenset(
        {"paused", "recovering", "verifying", "succeeded", "failed", "exhausted", "cancelled"}
    ),
    "paused": frozenset({"running", "cancelled", "failed"}),
    "recovering": frozenset({"running", "failed", "cancelled"}),
    "verifying": frozenset({"succeeded", "failed", "exhausted", "cancelled"}),
    "succeeded": frozenset(),
    "failed": frozenset(),
    "exhausted": frozenset(),
    "cancelled": frozenset(),
}


class RunLifecycle:
    """Pure run phase machine; callers own event normalization and side effects."""

    def __init__(self) -> None:
        self._phase: RunPhase = "created"

    @property
    def phase(self) -> RunPhase:
        return self._phase

    @property
    def terminal(self) -> bool:
        return self._phase in _TERMINAL_PHASES

    def transition(self, target: RunPhase) -> RunPhase:
        if target not in _TRANSITIONS[self._phase]:
            if self.terminal:
                raise ValueError(f"terminal run phase {self._phase} cannot transition to {target}")
            raise ValueError(f"invalid run transition: {self._phase} -> {target}")
        self._phase = target
        return self._phase

    def settle(
        self,
        *,
        succeeded: bool,
        exhausted: bool = False,
        cancelled: bool = False,
    ) -> RunPhase:
        """Choose one terminal phase from a completed lifecycle outcome."""
        terminal = sum((succeeded, exhausted, cancelled))
        if terminal > 1:
            raise ValueError("run outcome cannot be both successful, exhausted, and cancelled")
        target: RunPhase = (
            "succeeded"
            if succeeded
            else "cancelled"
            if cancelled
            else "exhausted"
            if exhausted
            else "failed"
        )
        return self.transition(target)


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
