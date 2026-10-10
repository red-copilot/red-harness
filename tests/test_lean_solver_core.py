"""Minimal controller behavior without World models or planning."""

import asyncio

from harness.lean_solver import LeanSolver
from harness.progress import ProgressLedger
from harness.session import AgentEvent
from harness.solver_policy import SolverAction
from harness.trace import TraceRecorder


class Session:
    def __init__(self):
        self.feedback = []

    async def observe(self, observation):
        self.feedback.append(observation)


def test_lean_solver_deduplicates_and_persists(tmp_path):
    progress = ProgressLedger()
    loop = LeanSolver(
        progress=progress, run_dir=tmp_path,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task"),
        world_revision=7,
    )
    session = Session()
    event = AgentEvent(type="tool.call", data={"tool": "read", "tool_call_id": "call-1"},
                       event_id="event-1")
    first = asyncio.run(loop.process_event(session, event))
    repeated = asyncio.run(loop.process_event(session, event))
    assert first.action is SolverAction.CONTINUE
    assert repeated.reason == "duplicate_event"
    assert progress.tool_calls == 1
    assert loop.planned_world_revision == 7
    assert "event-1" in loop.processed_event_ids
    assert (tmp_path / "progress.json").exists()


def test_lean_solver_preserves_stop_and_rejects_conflicting_ids(tmp_path):
    progress = ProgressLedger(no_progress_count=6)
    loop = LeanSolver(progress=progress, run_dir=tmp_path,
                      trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task"))
    session = Session()
    stopped = asyncio.run(loop.process_event(
        session, AgentEvent(type="other", data={}, event_id="same")))
    assert stopped.action is SolverAction.STOP
    assert any(item.type == "solver.stop" for item in session.feedback)
    conflicting = asyncio.run(loop.process_event(
        session, AgentEvent(type="other", data={"different": True}, event_id="same")))
    assert conflicting.reason == "conflicting_event_id"
    assert progress.event_count == 1
    assert any(item.type == "event.id_conflict" for item in session.feedback)


def test_lean_solver_exposes_zero_world_projection_stats(tmp_path):
    loop = LeanSolver(
        progress=ProgressLedger(),
        run_dir=tmp_path,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task"),
    )
    assert loop.stats.aci_accepted == 0
    assert loop.stats.aci_rejected == 0
    assert loop.stats.aci_world_mutations == 0
    assert loop.stats.inbox_accepted == 0
    assert loop.stats.inbox_rejected == 0
