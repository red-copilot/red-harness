"""Minimal Pi-first event controller.

No World model, planner, ACI or skill registry is imported on this path.
Benchmark verification and lifecycle remain the responsibility of their
independent trusted adapters.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from .progress import ProgressLedger
from .session import AgentEvent, AgentObservation, AgentSession
from .solver_policy import SolverAction, SolverDecision, decide_solver_action
from .trace import TraceRecorder

MAX_EVENT_IDS = 50_000


@dataclass
class LeanStats:
    aci_accepted: int = 0
    aci_rejected: int = 0
    aci_world_mutations: int = 0
    inbox_accepted: int = 0
    inbox_rejected: int = 0
    duplicate_events: int = 0
    conflicting_event_ids: int = 0


class LeanSolver:
    """Durable budgets, event deduplication and stop decisions only."""

    planner_enabled = False
    world_context_enabled = False
    planner = None
    skills = ()

    def __init__(
        self,
        *,
        progress: ProgressLedger,
        run_dir: Path,
        trace: TraceRecorder,
        world_revision: int = 0,
        processed_event_ids: dict[str, str] | None = None,
        sync_agent_views: Callable[[], None] | None = None,
    ) -> None:
        self.stats = LeanStats()
        self.progress = progress
        self.progress_path = run_dir / "progress.json"
        self.trace = trace
        self.planned_world_revision = world_revision
        self.processed_event_ids = dict(processed_event_ids or {})
        self._sync = sync_agent_views

    def decide(
        self, *, event_type: str | None = None, force_replan: bool = False
    ) -> SolverDecision:
        decision = decide_solver_action(
            event_type=event_type,
            replan_reasons=self.progress.replan_reasons,
            no_progress_count=self.progress.no_progress_count,
            replans_used=self.progress.replan_count,
            remaining_budget_fraction=self.progress.remaining_budget_fraction,
            force_replan=force_replan,
        )
        if decision.action is SolverAction.REPLAN:
            return SolverDecision(SolverAction.CONTINUE, "replan_suppressed")
        return decision

    async def maybe_replan(self, session: AgentSession, *, force: bool = False) -> None:
        return None

    async def process_event(self, session: AgentSession, event: AgentEvent, **kwargs) -> SolverDecision:
        if event.event_id:
            fingerprint = hashlib.sha256(
                json.dumps(
                    {"type": event.type, "data": event.data},
                    sort_keys=True, separators=(",", ":"), default=str,
                ).encode("utf-8")
            ).hexdigest()
            previous = self.processed_event_ids.get(event.event_id)
            if previous is not None:
                if previous == fingerprint:
                    self.stats.duplicate_events += 1
                else:
                    self.stats.conflicting_event_ids += 1
                if previous != fingerprint:
                    self.trace.emit(
                        "event.id_conflict", actor="harness",
                        data={"event_id": event.event_id, "event_type": event.type},
                    )
                    await session.observe(
                        AgentObservation(
                            type="event.id_conflict",
                            data={"event_id": event.event_id, "event_type": event.type},
                        )
                    )
                decision = SolverDecision(
                    SolverAction.CONTINUE,
                    "duplicate_event" if previous == fingerprint else "conflicting_event_id",
                )
                return decision
            self.processed_event_ids[event.event_id] = fingerprint
            if len(self.processed_event_ids) > MAX_EVENT_IDS:
                self.processed_event_ids.pop(next(iter(self.processed_event_ids)))

        self.progress.record_event(event)
        self.progress.write(self.progress_path)
        if self._sync is not None:
            self._sync()
        decision = self.decide(event_type=event.type)
        if decision.action is SolverAction.STOP:
            await session.observe(
                AgentObservation(type="solver.stop", data={"reason": decision.reason})
            )
        self.trace.emit(
            "solver.decision", actor="harness",
            data={"action": decision.action.value, "reason": decision.reason,
                  "event_type": event.type, "event_id": event.event_id},
        )
        return decision

    def finish_ingest(self) -> None:
        """No World inbox to drain in the Pi-first runtime."""
