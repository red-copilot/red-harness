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
                source="agent",
                trusted=False,
                data={
                    "action_id": "action-admin",
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
                source="agent",
                trusted=False,
                data={
                    "id": "obs-admin",
                    "action_id": "action-admin",
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
    assert progress.last_verification["status"] == "verified"

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
                source="agent",
                trusted=False,
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

    updates = [item for item in session.feedback if item.type == "solver.plan.updated"]
    assert len(updates) == 2
    assert updates[-1].data["actions"][0]["skill_id"] == "alternate-path"
    assert updates[-1].data["plan_path"] == "plan.json"



def test_solver_loop_does_not_verify_or_mutate_world_on_tool_result(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-tool", "task-tool")
    session = FakeSession()
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="probe admin",
        planned_world_revision=world.snapshot.revision,
    )

    asyncio.run(
        loop.process_event(
            session,
            AgentEvent(
                type="action.intent",
                source="agent",
                trusted=False,
                data={
                    "action_id": "action-admin",
                    "description": "probe admin",
                    "expected_observations": ["admin endpoint exists"],
                },
            ),
        )
    )
    revision_before = world.snapshot.revision
    asyncio.run(
        loop.process_event(
            session,
            AgentEvent(
                type="tool.result",
                source="runtime",
                trusted=True,
                data={
                    "tool": "bash",
                    "tool_call_id": "call-1",
                    "is_error": False,
                },
            ),
        )
    )

    assert world.snapshot.revision == revision_before
    assert progress.last_verification is None
    assert not any(item.type == "solver.verification" for item in session.feedback)


def test_solver_loop_does_not_cross_verify_other_action_observation(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-bind", "task-bind")
    session = FakeSession()
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=tmp_path,
        trace=trace,
        actor="agent:test",
        context_query="probe admin",
        planned_world_revision=world.snapshot.revision,
    )

    asyncio.run(
        loop.process_event(
            session,
            AgentEvent(
                type="action.intent",
                source="agent",
                trusted=False,
                data={
                    "action_id": "action-current",
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
                source="agent",
                trusted=False,
                data={
                    "action_id": "action-other",
                    "type": "web.endpoint",
                    "summary": "admin endpoint exists",
                    "content": {"path": "/admin"},
                },
            ),
        )
    )

    assert world.snapshot.observations
    assert progress.actual_observation is None
    assert progress.last_verification is None
    assert not any(item.type == "solver.verification" for item in session.feedback)
