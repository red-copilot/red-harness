from __future__ import annotations

import asyncio
from pathlib import Path

from harness.progress import ProgressLedger
from harness.session import AgentEvent
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
