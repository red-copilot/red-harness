from __future__ import annotations

import hashlib
import json
import uuid
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import ClassVar

from .aci import TypedACI
from .action_verifier import ActionVerifier
from .planner import RollingHorizonPlanner, RollingPlan
from .progress import ProgressLedger
from .session import AgentEvent, AgentObservation, AgentSession
from .skills import SkillSpec
from .solver_policy import SolverAction, SolverDecision, decide_solver_action
from .trace import TraceRecorder

MAX_PROCESSED_EVENT_IDS = 50_000
from .verification import AuthorizedActionVerdict, VerifierRegistry
from .world import Observation, Provenance, WorldContextBuilder, WorldRepository
from .world.live import WorldInboxCursor


@dataclass
class SolverLoopStats:
    aci_accepted: int = 0
    aci_rejected: int = 0
    aci_world_mutations: int = 0
    inbox_accepted: int = 0
    inbox_rejected: int = 0
    duplicate_events: int = 0
    conflicting_event_ids: int = 0
    last_verification_key: tuple | None = None


class SolverLoop:
    """Shared state/verification loop for interactive AgentSession execution."""

    VERIFY_EVENTS: ClassVar[frozenset[str]] = frozenset(
        {"tool.result", "progress.updated", "world.observe"}
    )

    def __init__(
        self,
        *,
        world: WorldRepository,
        progress: ProgressLedger,
        run_dir: Path,
        trace: TraceRecorder,
        actor: str,
        context_query: str,
        planned_world_revision: int,
        typed_aci: TypedACI | None = None,
        action_verifier: ActionVerifier | None = None,
        verifier_registry: VerifierRegistry | None = None,
        context_builder: WorldContextBuilder | None = None,
        world_inbox: WorldInboxCursor | None = None,
        skills: list[SkillSpec] | None = None,
        planner: RollingHorizonPlanner | None = None,
        planner_enabled: bool = True,
        world_context_enabled: bool = True,
        plan_horizon: int = 3,
        processed_event_ids: dict[str, str] | None = None,
        world_inbox_path: Path | None = None,
        sync_agent_views: Callable[[], None] | None = None,
    ) -> None:
        self.world = world
        self.progress = progress
        self.run_dir = run_dir
        self.trace = trace
        self.actor = actor
        self.context_query = context_query
        self.planned_world_revision = planned_world_revision
        self.typed_aci = typed_aci or TypedACI()
        self.verifier_registry = verifier_registry or VerifierRegistry()
        self.action_verifier = action_verifier or ActionVerifier(self.verifier_registry)
        self.context_builder = context_builder or WorldContextBuilder()
        self.world_inbox = world_inbox or WorldInboxCursor(
            world_inbox_path or run_dir / "world.inbox.jsonl",
            world,
            actor=actor,
        )
        self.progress_path = run_dir / "progress.json"
        self.sync_agent_views = sync_agent_views
        self.plan_path = run_dir / "plan.json"
        self.skills = list(skills or [])
        self.planner = (planner or RollingHorizonPlanner()) if planner_enabled else None
        self.planner_enabled = planner_enabled
        self.world_context_enabled = world_context_enabled
        self.plan_horizon = max(1, min(3, plan_horizon))
        self._last_plan_signature: tuple | None = None
        self.stats = SolverLoopStats()
        self._processed_event_ids: dict[str, str] = dict(processed_event_ids or {})

    def _correlate_legacy_tool_event(self, event: AgentEvent) -> AgentEvent:
        if event.type not in {"tool.call", "tool.result"}:
            return event
        data = dict(event.data)
        event_id = event.event_id
        call_id = data.get("tool_call_id")
        valid_call_id = isinstance(call_id, str) and 0 < len(call_id) <= 256
        correlations = self.progress.tool_event_correlations
        if not valid_call_id and isinstance(event_id, str):
            call_id = correlations.get(event_id)
            valid_call_id = isinstance(call_id, str) and 0 < len(call_id) <= 256

        if not valid_call_id and event.type == "tool.call":
            if isinstance(event_id, str) and event_id:
                seed = event_id.encode("utf-8")
                call_id = f"legacy_{hashlib.sha256(seed).hexdigest()[:32]}"
            else:
                call_id = f"legacy_{uuid.uuid4().hex}"
            valid_call_id = True
        elif not valid_call_id and event.type == "tool.result":
            tool = data.get("tool")
            candidates = [
                action
                for action in self.progress.active_actions.values()
                if not isinstance(tool, str) or action.get("tool") == tool
            ]
            if len(candidates) == 1:
                candidate_id = candidates[0].get("tool_call_id")
                if isinstance(candidate_id, str) and candidate_id:
                    call_id = candidate_id
                    valid_call_id = True

        if valid_call_id:
            data["tool_call_id"] = call_id
            if isinstance(event_id, str) and event_id:
                correlations[event_id] = call_id
                if len(correlations) > 100_000:
                    correlations.pop(next(iter(correlations)))
        return AgentEvent(type=event.type, data=data, event_id=event.event_id)

    async def process_event(
        self,
        session: AgentSession,
        event: AgentEvent,
        *,
        trusted_verdict: AuthorizedActionVerdict | None = None,
    ) -> SolverDecision:
        event = self._correlate_legacy_tool_event(event)
        if event.event_id:
            canonical = json.dumps(
                {"type": event.type, "data": event.data},
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                default=str,
            )
            fingerprint = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
            previous = self._processed_event_ids.get(event.event_id)
            if previous == fingerprint:
                self.stats.duplicate_events += 1
                decision = SolverDecision(SolverAction.CONTINUE, "duplicate_event")
                self._emit_decision(decision, event_type=event.type, event_id=event.event_id)
                return decision
            if previous is not None:
                self.stats.conflicting_event_ids += 1
                self.trace.emit(
                    "event.id_conflict",
                    actor="harness",
                    data={"event_id": event.event_id, "event_type": event.type},
                )
                await session.observe(
                    AgentObservation(
                        type="event.id_conflict",
                        data={"event_id": event.event_id, "event_type": event.type},
                    )
                )
                decision = SolverDecision(SolverAction.CONTINUE, "conflicting_event_id")
                self._emit_decision(decision, event_type=event.type, event_id=event.event_id)
                return decision
            self._processed_event_ids[event.event_id] = fingerprint
            if len(self._processed_event_ids) > MAX_PROCESSED_EVENT_IDS:
                self._processed_event_ids.pop(next(iter(self._processed_event_ids)))

        self.progress.record_event(event)
        self.progress.write(self.progress_path)
        self._sync_agent_views()
        before_revision = self.world.snapshot.revision
        if event.type in {"tool.call", "tool.result"}:
            tool_call_id = event.data.get("tool_call_id")
            if isinstance(tool_call_id, str) and 0 < len(tool_call_id) <= 256:
                trace_data: dict[str, object] = {
                    "tool_call_id": tool_call_id,
                    "source_event_id": (
                        event.event_id
                        if isinstance(event.event_id, str) and len(event.event_id) <= 256
                        else None
                    ),
                }
                if event.type == "tool.result":
                    trace_data["is_error"] = bool(event.data.get("is_error", False))
                    trace_data["execution_status"] = (self.progress.last_action or {}).get("status")
                self.trace.emit(
                    event.type,
                    actor=self.actor,
                    data=trace_data,
                    parent_event_id=event.event_id,
                    action_id=tool_call_id,
                )

        try:
            aci_mutations = self.typed_aci.apply(
                event,
                world=self.world,
                progress=self.progress,
                actor=self.actor,
            )
        except (KeyError, TypeError, ValueError) as exc:
            aci_mutations = 0
            self.stats.aci_rejected += 1
            self.trace.emit(
                "aci.rejected",
                actor="harness",
                data={
                    "event_type": event.type,
                    "error_type": type(exc).__name__,
                    "message": str(exc),
                },
            )
            await session.observe(
                AgentObservation(
                    type="aci.feedback",
                    data={
                        "accepted": False,
                        "event_type": event.type,
                        "message": str(exc),
                    },
                )
            )
        else:
            if event.type.startswith(("world.", "action.")):
                self.stats.aci_accepted += 1
                await session.observe(
                    AgentObservation(
                        type="aci.feedback",
                        data={
                            "accepted": True,
                            "event_type": event.type,
                            "world_mutations": aci_mutations,
                        },
                    )
                )
            self.stats.aci_world_mutations += aci_mutations
            if aci_mutations:
                self.trace.emit(
                    "aci.applied",
                    actor="harness",
                    data={
                        "event_type": event.type,
                        "mutations": aci_mutations,
                        "revision_before": before_revision,
                        "revision_after": self.world.snapshot.revision,
                    },
                )

        live_ingest = self.world_inbox.poll()
        self.stats.inbox_accepted += live_ingest.accepted
        self.stats.inbox_rejected += live_ingest.rejected
        semantic_agent_update = event.type in {
            "world.observe",
            "world.hypothesis",
            "world.capability",
            "world.artifact",
            "world.failure",
        }
        if self.planner_enabled and ((semantic_agent_update and aci_mutations) or live_ingest.accepted):
            self.progress.request_replan("world_state_changed")
        after_revision = self.world.snapshot.revision
        if live_ingest.accepted or live_ingest.rejected:
            self.trace.emit(
                "world.ingested",
                data={
                    "accepted": live_ingest.accepted,
                    "rejected": live_ingest.rejected,
                    "errors": [error.model_dump() for error in live_ingest.errors[:10]],
                    "revision_before": before_revision,
                    "revision_after": after_revision,
                    "live": True,
                },
            )

        if after_revision != before_revision:
            self._write_context()
            self.trace.emit(
                "world.state.updated",
                data={
                    "revision_before": before_revision,
                    "revision_after": after_revision,
                },
            )
            await session.observe(
                AgentObservation(
                    type="world.state.updated",
                    data={
                        "revision_before": before_revision,
                        "revision_after": after_revision,
                        "context_path": "world.context.txt",
                    },
                )
            )

        if event.type in self.VERIFY_EVENTS:
            verification = self.action_verifier.verify(
                self.progress,
                run_id=self.trace.run_id,
                planned_world_revision=self.planned_world_revision,
                current_world_revision=self.world.snapshot.revision,
                evidence_world_revision=before_revision,
                trusted_verdict=trusted_verdict,
            )
            if verification is None:
                self.progress.skipped_verifications += 1
            else:
                verification_key = (
                    verification.status,
                    verification.expected_observation,
                    verification.actual_observation,
                    tuple(verification.replan_reasons),
                    (self.progress.last_action or {}).get("tool_call_id"),
                    (self.progress.last_action or {}).get("status"),
                )
                if verification_key != self.stats.last_verification_key:
                    self.stats.last_verification_key = verification_key
                    self.progress.record_verification(verification)
                    if verification.status == "verified":
                        if self.planner_enabled:
                            self.progress.request_replan("evidence_confirmed")
                        action_id = (self.progress.last_action or {}).get("tool_call_id")
                        if isinstance(action_id, str) and action_id:
                            revision_before_verification = self.world.snapshot.revision
                            self.world.upsert(
                                "observation",
                                Observation(
                                    id=f"verified-action:{action_id}",
                                    type="action.verified",
                                    content={
                                        "action_id": action_id,
                                        "expected_observation": verification.expected_observation,
                                        "actual_observation": verification.actual_observation,
                                        "evidence": [
                                            ref.model_dump(mode="json")
                                            for ref in verification.evidence
                                        ],
                                    },
                                    confidence=1.0,
                                    source="harness.action_verifier",
                                    provenance=Provenance(epistemic_status="verified"),
                                ),
                                actor="harness",
                                source_event_id=event.event_id,
                            )
                            self._write_context()
                            self.trace.emit(
                                "world.state.updated",
                                data={
                                    "revision_before": revision_before_verification,
                                    "revision_after": self.world.snapshot.revision,
                                    "cause": "action.verified",
                                },
                            )
                    self.trace.emit(
                        "action.verified",
                        actor="harness",
                        data=verification.model_dump(mode="json"),
                    )
                    await session.observe(
                        AgentObservation(
                            type="solver.verification",
                            data=verification.model_dump(mode="json"),
                        )
                    )

        self.progress.write(self.progress_path)
        self._sync_agent_views()
        decision = self.decide(event_type=event.type)
        if decision.action is SolverAction.REPLAN:
            plan = await self.maybe_replan(session)
            if plan is None:
                decision = SolverDecision(SolverAction.CONTINUE, "replan_suppressed")
        if decision.action is SolverAction.STOP:
            await session.observe(
                AgentObservation(
                    type="solver.stop",
                    data={"reason": decision.reason},
                )
            )
        self._emit_decision(decision, event_type=event.type, event_id=event.event_id)
        return decision

    def _emit_decision(
        self, decision: SolverDecision, *, event_type: str, event_id: str | None
    ) -> None:
        self.trace.emit(
            "solver.decision",
            actor="harness",
            data={
                "action": decision.action.value,
                "reason": decision.reason,
                "event_type": event_type,
                "event_id": event_id,
                "world_revision": self.world.snapshot.revision,
                "no_progress_count": self.progress.no_progress_count,
                "replan_count": self.progress.replan_count,
                "remaining_budget_fraction": self.progress.remaining_budget_fraction,
            },
        )

    @property
    def processed_event_ids(self) -> dict[str, str]:
        return dict(self._processed_event_ids)

    async def maybe_replan(
        self,
        session: AgentSession,
        *,
        force: bool = False,
    ) -> RollingPlan | None:
        if not self.planner_enabled:
            return None
        decision = self.decide(force_replan=force)
        if decision.action is not SolverAction.REPLAN:
            return None
        reasons = list(self.progress.replan_reasons)
        if self.progress.no_progress_count >= 2 and "no_progress_threshold" not in reasons:
            reasons.append("no_progress_threshold")
        if not force and not reasons:
            return None

        signature = (
            self.world.snapshot.revision,
            tuple(reasons),
            self.progress.no_progress_count,
            (self.progress.last_verification or {}).get("status"),
        )
        if not force and signature == self._last_plan_signature:
            return None

        plan = self.planner.propose(
            self.world.snapshot,
            self.skills,
            progress=self.progress,
            horizon=self.plan_horizon,
        )
        self.plan_path.write_text(
            plan.model_dump_json(indent=2) + "\n",
            encoding="utf-8",
        )
        self.planned_world_revision = self.world.snapshot.revision
        self.progress.replan_count += 1
        payload = plan.model_dump(mode="json")
        self.trace.emit(
            "solver.plan.updated",
            actor="harness",
            data=payload,
        )
        # Commit the complete planner transition once, before exposing it to Pi.
        # Previously we persisted and synced an intermediate progress state, then
        # consumed the reasons and wrote again without syncing the final state.
        self.progress.consume_replan_reasons(reasons)
        self.progress.write(self.progress_path)
        self._sync_agent_views()
        await session.observe(
            AgentObservation(
                type="solver.plan.updated",
                data={
                    **payload,
                    "plan_path": "plan.json",
                },
            )
        )
        self._last_plan_signature = signature
        return plan

    def decide(
        self,
        *,
        event_type: str | None = None,
        force_replan: bool = False,
    ) -> SolverDecision:
        return decide_solver_action(
            event_type=event_type,
            replan_reasons=self.progress.replan_reasons,
            no_progress_count=self.progress.no_progress_count,
            replans_used=self.progress.replan_count,
            remaining_budget_fraction=self.progress.remaining_budget_fraction,
            force_replan=force_replan,
        )

    def finish_ingest(self) -> None:
        report = self.world_inbox.poll()
        self.stats.inbox_accepted += report.accepted
        self.stats.inbox_rejected += report.rejected
        self.trace.emit(
            "world.ingested",
            data={
                "accepted": report.accepted,
                "rejected": report.rejected,
                "errors": [error.model_dump() for error in report.errors[:10]],
            },
        )

    def _write_context(self) -> None:
        (self.run_dir / "world.context.txt").write_text(
            self.context_builder.render(self.world.snapshot, query=self.context_query)
            if self.world_context_enabled
            else "",
            encoding="utf-8",
        )
        self._sync_agent_views()

    def _sync_agent_views(self) -> None:
        if self.sync_agent_views is not None:
            self.sync_agent_views()
