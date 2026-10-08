from __future__ import annotations

import asyncio
from pathlib import Path

from harness.progress import ProgressLedger
from harness.session import AgentEvent
from harness.skills import SkillSpec
from harness.solver_loop import SolverLoop
from harness.trace import TraceRecorder
from harness.world import SQLiteWorldRepository


class FakeSession:
    def __init__(self) -> None:
        self.feedback = []

    async def observe(self, observation) -> None:
        self.feedback.append(observation)


def test_solver_loop_applies_aci_updates_world_and_verifies(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-test", "task-test")
    session = FakeSession()
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="find admin endpoint",
        planned_world_revision=world.snapshot.revision,
    )

    asyncio.run(
        loop.process_event(
            session,
            AgentEvent(
                type="action.intent",
                data={
                    "description": "probe admin",
                    "expected_observations": ["admin endpoint exists"],
                },
            ),
        )
    )
    asyncio.run(
        loop.process_event(
            session,
            AgentEvent(
                type="world.observe",
                data={
                    "id": "obs-admin",
                    "type": "web.endpoint",
                    "summary": "admin endpoint exists",
                    "content": {"path": "/admin", "status": 200},
                    "confidence": 1.0,
                },
            ),
        )
    )

    assert "obs-admin" in world.snapshot.observations
    assert loop.stats.aci_accepted == 2
    assert loop.stats.aci_world_mutations == 1
    assert progress.last_verification is not None
    assert progress.last_verification["status"] == "pending"

    feedback_types = [item.type for item in session.feedback]
    assert "aci.feedback" in feedback_types
    assert "world.state.updated" in feedback_types
    assert "solver.verification" in feedback_types
    assert "obs-admin" in (tmp_path / "world.context.txt").read_text(encoding="utf-8")


def test_solver_loop_rejects_invalid_typed_event_without_stopping(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-test", "task-test")
    session = FakeSession()
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="test invalid event",
        planned_world_revision=world.snapshot.revision,
    )

    asyncio.run(
        loop.process_event(
            session,
            AgentEvent(
                type="world.observe",
                data={"id": "bad-observation"},
            ),
        )
    )

    assert loop.stats.aci_rejected == 1
    assert world.snapshot.revision == 0
    rejected = next(
        item
        for item in session.feedback
        if item.type == "aci.feedback" and item.data["accepted"] is False
    )
    assert rejected.data["event_type"] == "world.observe"


def test_solver_loop_publishes_initial_and_replan_updates(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-plan", "task-plan")
    session = FakeSession()
    skill = SkillSpec(
        id="alternate-path",
        description="Try an alternate path",
        produces=[{"kind": "observation", "type": "alternate.evidence"}],
    )
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="solve the target",
        planned_world_revision=world.snapshot.revision,
        skills=[skill],
        plan_horizon=1,
    )

    initial = asyncio.run(loop.maybe_replan(session, force=True))
    assert initial is not None
    assert initial.actions[0].skill_id == "alternate-path"
    assert (tmp_path / "plan.json").is_file()

    progress.record_submission(accepted=False, completed=False)
    replanned = asyncio.run(loop.maybe_replan(session))
    assert replanned is not None
    assert replanned.replan_required is True
    assert "benchmark_negative_feedback" in replanned.replan_reasons
    assert progress.replan_reasons == []

    updates = [item for item in session.feedback if item.type == "solver.plan.updated"]
    assert len(updates) == 2
    assert updates[-1].data["actions"][0]["skill_id"] == "alternate-path"
    assert updates[-1].data["plan_path"] == "plan.json"


def test_solver_loop_uses_separate_trusted_evidence_input(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-verified", "task-verified")
    session = FakeSession()
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="verify the service",
        planned_world_revision=world.snapshot.revision,
    )

    async def run() -> None:
        await loop.process_event(
            session,
            AgentEvent(
                type="action.intent",
                data={"expected_observations": ["service is reachable"]},
            ),
        )
        await loop.process_event(
            session,
            AgentEvent(
                type="tool.call",
                data={"tool": "probe", "tool_call_id": "call-1"},
            ),
        )
        await loop.process_event(
            session,
            AgentEvent(
                type="tool.result",
                data={"tool": "probe", "tool_call_id": "call-1", "is_error": False},
            ),
            trusted_evidence={
                "source": "tool_adapter",
                "verdict": "verified",
                "action_id": "call-1",
                "evidence_id": "probe-result-1",
                "observation": "service answered the independent probe",
            },
        )

    asyncio.run(run())

    assert progress.last_verification["status"] == "verified"
    assert progress.evidence_confirmed == 1
    assert progress.no_progress_count == 0
    assert "probe-result-1" in progress.last_verification["evidence"][0]
    assert (
        world.snapshot.observations["verified-action:call-1"].provenance.epistemic_status
        == "verified"
    )


def test_solver_loop_deduplicates_identical_event_ids_and_rejects_conflicts(
    tmp_path: Path,
) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-dedupe", "task-dedupe")
    session = FakeSession()
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="dedupe events",
        planned_world_revision=world.snapshot.revision,
    )
    event = AgentEvent(
        type="progress.updated",
        data={"confirmed_fact": "the host is reachable"},
        event_id="event-1",
    )

    async def run() -> None:
        await loop.process_event(session, event)
        await loop.process_event(session, event)
        await loop.process_event(
            session,
            AgentEvent(
                type="progress.updated",
                data={"confirmed_fact": "a different claim"},
                event_id="event-1",
            ),
        )

    asyncio.run(run())

    assert progress.claims == ["the host is reachable"]
    assert loop.stats.duplicate_events == 1
    assert loop.stats.conflicting_event_ids == 1
