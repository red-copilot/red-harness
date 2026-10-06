from __future__ import annotations

import asyncio
import json
import time
from pathlib import Path

from redharness.agent import AgentResult
from redharness.budget import UsageMetrics
from redharness.failures import FailureClass, classify_failure
from redharness.session import AgentObservation, OneShotAgentSession
from redharness.trace import TraceRecorder


class FakeStreamingAdapter:
    def run(self, *, run_dir: Path, **_kwargs) -> AgentResult:
        event_path = run_dir / "events.jsonl"
        event_path.write_text("", encoding="utf-8")
        for index in range(2):
            with event_path.open("a", encoding="utf-8") as handle:
                handle.write(
                    json.dumps(
                        {
                            "type": "progress.updated",
                            "data": {"step": index + 1},
                        }
                    )
                    + "\n"
                )
                handle.flush()
            time.sleep(0.05)
        return AgentResult(
            returncode=0,
            timed_out=False,
            budget_exceeded=None,
            stdout="",
            stderr="",
            metrics=UsageMetrics(),
        )


def test_trace_emits_causal_identifiers(tmp_path: Path) -> None:
    recorder = TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test")
    parent = recorder.emit("plan.created", plan_id="plan-1")
    child = recorder.emit(
        "action.started",
        parent_event_id=parent,
        plan_id="plan-1",
        action_id="action-1",
    )

    rows = [
        json.loads(line)
        for line in (tmp_path / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert rows[0]["event_id"] == parent
    assert rows[1]["event_id"] == child
    assert rows[1]["parent_event_id"] == parent
    assert rows[1]["plan_id"] == "plan-1"
    assert rows[1]["action_id"] == "action-1"


def test_failure_taxonomy_classifies_execution_boundaries() -> None:
    assert classify_failure(timed_out=True) == FailureClass.TOOL_TIMEOUT
    assert (
        classify_failure(budget_exceeded="max_tokens")
        == FailureClass.BUDGET_EXHAUSTED
    )
    assert classify_failure(returncode=2) == FailureClass.TOOL_ERROR
    assert classify_failure(returncode=0) is None


def test_oneshot_agent_session_streams_events_and_feedback(tmp_path: Path) -> None:
    async def scenario() -> None:
        session = await OneShotAgentSession.start(
            FakeStreamingAdapter(),
            run_dir=tmp_path,
            run_kwargs={"run_dir": tmp_path},
        )
        events = [event async for event in session.events()]
        assert [event.data["step"] for event in events] == [1, 2]

        await session.observe(
            AgentObservation(
                type="benchmark.feedback",
                data={"accepted": True, "progress": 1},
            )
        )
        checkpoint = await session.checkpoint()
        assert checkpoint.event_offset > 0
        assert checkpoint.feedback_offset > 0

        await session.close("objective-complete")
        result = await session.result()
        assert result.returncode == 0

        feedback = (tmp_path / "agent.feedback.jsonl").read_text(encoding="utf-8")
        assert "benchmark.feedback" in feedback
        control = (tmp_path / "agent.control.jsonl").read_text(encoding="utf-8")
        assert "objective-complete" in control

    asyncio.run(scenario())
