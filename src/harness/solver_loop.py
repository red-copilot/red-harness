from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import ClassVar

from .aci import TypedACI
from .action_verifier import ActionVerifier
from .planner import RollingHorizonPlanner, RollingPlan
from .progress import ProgressLedger
from .session import AgentEvent, AgentObservation, AgentSession
from .skills import SkillSpec
from .trace import TraceRecorder
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
        context_builder: WorldContextBuilder | None = None,
        world_inbox: WorldInboxCursor | None = None,
        skills: list[SkillSpec] | None = None,
        planner: RollingHorizonPlanner | None = None,
        plan_horizon: int = 3,
        processed_event_ids: dict[str, str] | None = None,
    ) -> None:
        self.world = world
        self.progress = progress
        self.run_dir = run_dir
        self.trace = trace
        self.actor = actor
        self.context_query = context_query
        self.planned_world_revision = planned_world_revision
        self.typed_aci = typed_aci or TypedACI()
        self.action_verifier = action_verifier or ActionVerifier()
        self.context_builder = context_builder or WorldContextBuilder()
        self.world_inbox = world_inbox or WorldInboxCursor(
            run_dir / "world.inbox.jsonl",
            world,
            actor=actor,
        )
        self.progress_path = run_dir / "progress.json"
        self.plan_path = run_dir / "plan.json"
        self.skills = list(skills or [])
        self.planner = planner or RollingHorizonPlanner()
        self.plan_horizon = max(1, min(3, plan_horizon))
        self._last_plan_signature: tuple | None = None
        self.stats = SolverLoopStats()
        self._processed_event_ids: dict[str, str] = dict(processed_event_ids or {})

    async def process_event(
        self,
        session: AgentSession,
        event: AgentEvent,
        *,
        trusted_evidence: dict | None = None,
    ) -> None:
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
                return
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
                return
            self._processed_event_ids[event.event_id] = fingerprint
            if len(self._processed_event_ids) > 100_000:
                self._processed_event_ids.pop(next(iter(self._processed_event_ids)))

        self.progress.record_event(event)
        before_revision = self.world.snapshot.revision

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
        if (semantic_agent_update and aci_mutations) or live_ingest.accepted:
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
                planned_world_revision=self.planned_world_revision,
                current_world_revision=self.world.snapshot.revision,
                trusted_evidence=trusted_evidence,
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
                                        "evidence": verification.evidence,
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
        await self.maybe_replan(session)

    @property
    def processed_event_ids(self) -> dict[str, str]:
        return dict(self._processed_event_ids)

    async def maybe_replan(
        self,
        session: AgentSession,
        *,
        force: bool = False,
    ) -> RollingPlan | None:
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
        payload = plan.model_dump(mode="json")
        self.trace.emit(
            "solver.plan.updated",
            actor="harness",
            data=payload,
        )
        await session.observe(
            AgentObservation(
                type="solver.plan.updated",
                data={
                    **payload,
                    "plan_path": "plan.json",
                },
            )
        )
        self.progress.consume_replan_reasons(reasons)
        self.progress.write(self.progress_path)
        self._last_plan_signature = signature
        return plan

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
            self.context_builder.render(
                self.world.snapshot,
                query=self.context_query,
            ),
            encoding="utf-8",
        )
