from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .aci import TypedACI
from .action_verifier import ActionVerifier
from .planner import RollingHorizonPlanner, RollingPlan
from .progress import ProgressLedger
from .session import AgentEvent, AgentObservation, AgentSession
from .skills import SkillSpec
from .trace import TraceRecorder
from .world import WorldContextBuilder, WorldRepository
from .world.live import WorldInboxCursor


@dataclass
class SolverLoopStats:
    aci_accepted: int = 0
    aci_rejected: int = 0
    aci_world_mutations: int = 0
    inbox_accepted: int = 0
    inbox_rejected: int = 0
    last_verification_key: tuple | None = None


class SolverLoop:
    """Shared state/verification loop for interactive AgentSession execution."""


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

    async def process_event(self, session: AgentSession, event: AgentEvent) -> None:
        self.progress.record_event(event)
        before_revision = self.world.snapshot.revision

        if event.type == "agent.event_rejected":
            self.stats.aci_rejected += 1
            self.trace.emit(
                "aci.rejected",
                actor="harness",
                data={
                    "event_type": event.data.get("event_type"),
                    "error_type": "UntrustedEventType",
                    "message": event.data.get("reason"),
                },
            )
            await session.observe(
                AgentObservation(
                    type="aci.feedback",
                    data={
                        "accepted": False,
                        "event_type": event.data.get("event_type"),
                        "message": event.data.get("reason"),
                    },
                )
            )
            self.progress.write(self.progress_path)
            return

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
        after_revision = self.world.snapshot.revision
        if live_ingest.accepted or live_ingest.rejected:
            self.trace.emit(
                "world.ingested",
                data={
                    "accepted": live_ingest.accepted,
                    "rejected": live_ingest.rejected,
                    "errors": [
                        error.model_dump()
                        for error in live_ingest.errors[:10]
                    ],
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

        should_verify = (
            event.type == "world.observe"
            or (
                event.type == "progress.updated"
                and isinstance(event.data.get("actual_observation"), str)
                and bool(event.data.get("actual_observation"))
            )
        )
        if should_verify:
            verification = self.action_verifier.verify(
                self.progress,
                planned_world_revision=self.progress.intent_world_revision,
                current_world_revision=(
                    before_revision
                    if event.type == "world.observe"
                    else self.world.snapshot.revision
                ),
            )
            if verification is None:
                self.progress.skipped_verifications += 1
            else:
                action_id = (self.progress.action_intent or {}).get("action_id")
                verification_key = (
                    action_id,
                    verification.status,
                    verification.expected_observation,
                    verification.actual_observation,
                    tuple(verification.replan_reasons),
                )
                if verification_key != self.stats.last_verification_key:
                    self.stats.last_verification_key = verification_key
                    self.progress.record_verification(verification)
                    self.trace.emit(
                        "action.verified",
                        actor="harness",
                        action_id=action_id if isinstance(action_id, str) else None,
                        data=verification.model_dump(mode="json"),
                    )
                    await session.observe(
                        AgentObservation(
                            type="solver.verification",
                            data={
                                **verification.model_dump(mode="json"),
                                "action_id": action_id,
                            },
                        )
                    )

        self.progress.write(self.progress_path)
        await self.maybe_replan(session)

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
        self._last_plan_signature = signature
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
