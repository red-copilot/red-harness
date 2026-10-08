from __future__ import annotations

import asyncio
import json
from pathlib import Path

from harness.agent import AgentResult
from harness.benchmark.base import (
    BenchmarkCase,
    BenchmarkSession,
    EvaluationResult,
    SubmissionResult,
)
from harness.benchmark.runner import BenchmarkRunner
from harness.budget import UsageMetrics
from harness.models import AgentSpec, BudgetSpec, ObjectiveSpec
from harness.session import AgentEvent


class FakeBenchmarkAdapter:
    def __init__(self) -> None:
        self.submissions: list[str] = []
        self.closed = False

    async def provision(self, case: BenchmarkCase) -> BenchmarkSession:
        return BenchmarkSession(
            id="session-1",
            benchmark=case.benchmark,
            case_id=case.id,
            objective=ObjectiveSpec(description="submit one flag"),
        )

    async def submit(self, _session, submission):
        self.submissions.append(submission.value)
        return SubmissionResult(
            accepted=True,
            score_delta=10,
            completed=True,
            metadata={"correct_flag_count": 1, "total_flag_count": 1},
        )

    async def evaluate(self, _session):
        return EvaluationResult(success=True, score=10)

    async def teardown(self, _session):
        self.closed = True


class FakeSession:
    def __init__(self, run_dir: Path) -> None:
        self.run_dir = run_dir
        self.feedback = []
        self.closed_reason = None

    async def events(self):
        yield AgentEvent(
            type="action.intent",
            data={
                "action_id": "action-probe-admin",
                "description": "probe admin endpoint",
                "expected_observations": ["admin endpoint exists"],
                "success_conditions": ["HTTP response proves /admin is routed"],
                "replan_conditions": ["endpoint absent"],
            },
        )
        yield AgentEvent(
            type="world.observe",
            data={
                "id": "obs-live",
                "type": "web.endpoint",
                "summary": "admin endpoint exists at /admin",
                "content": {"path": "/admin", "status": 200},
                "confidence": 1.0,
            },
        )
        path = self.run_dir / "submission.inbox.jsonl"
        path.write_text(
            json.dumps({"type": "flag", "value": "flag{live}"}) + "\n",
            encoding="utf-8",
        )
        yield AgentEvent(
            type="progress.updated",
            data={"step": 1, "made_progress": True},
        )

    async def observe(self, observation):
        self.feedback.append(observation)

    async def close(self, reason: str):
        self.closed_reason = reason

    async def result(self):
        return AgentResult(
            returncode=0,
            timed_out=False,
            budget_exceeded=None,
            stdout="",
            stderr="",
            metrics=UsageMetrics(),
        )


class FakeAgentAdapter:
    def __init__(self) -> None:
        self.session: FakeSession | None = None

    async def start_session(self, *, run_dir: Path, **_kwargs):
        self.session = FakeSession(run_dir)
        return self.session


def test_benchmark_runner_streams_submission_and_feedback(monkeypatch, tmp_path: Path) -> None:
    import harness.benchmark.runner as runner_module

    fake_agent = FakeAgentAdapter()
    monkeypatch.setattr(
        runner_module,
        "build_agent_adapter",
        lambda *_args, **_kwargs: fake_agent,
    )
    benchmark = FakeBenchmarkAdapter()
    case = BenchmarkCase(id="CASE-1", benchmark="fake")
    agent = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "demo",
            "type": "cli",
            "command": ["true"],
        }
    )

    result = asyncio.run(
        BenchmarkRunner(runs_root=tmp_path).run_case(
            adapter=benchmark,
            case=case,
            agent=agent,
            budgets=BudgetSpec(wall_time=30),
            seed=1,
            submission_extractor=lambda _result: [],
            allow_host_agent=True,
        )
    )

    assert result["success"] is True
    assert benchmark.submissions == ["flag{live}"]
    assert benchmark.closed is True
    assert fake_agent.session is not None
    assert fake_agent.session.closed_reason == "objective-complete"
    feedback_types = [item.type for item in fake_agent.session.feedback]
    assert "world.state.updated" in feedback_types
    assert "solver.verification" in feedback_types
    assert "benchmark.feedback" in feedback_types
    benchmark_feedback = next(
        item for item in fake_agent.session.feedback if item.type == "benchmark.feedback"
    )
    assert benchmark_feedback.data["accepted"] is True
    assert benchmark_feedback.data["completed"] is True
    assert benchmark_feedback.data["world_revision"] >= 1
    assert benchmark_feedback.data["replan_required"] is False
    assert result["progress"]["verified_actions"] == 0
    assert result["progress"]["pending_actions"] >= 1
    assert result["progress"]["skipped_verifications"] >= 0
    assert result["progress"]["last_verification"]["status"] == "pending"
    assert result["progress"]["accepted_submissions"] == 1
    assert result["progress"]["objective_completed"] is True
    assert result["world"]["revision"] >= 2
    context = next(tmp_path.glob("fake_CASE-1_*/world.context.txt")).read_text(
        encoding="utf-8"
    )
    assert "obs-live" in context
    assert "benchmark.submission.feedback" in context


class ReactiveBenchmarkAdapter(FakeBenchmarkAdapter):
    async def submit(self, _session, submission):
        self.submissions.append(submission.value)
        accepted = submission.value == "flag{good}"
        return SubmissionResult(
            accepted=accepted,
            score_delta=10 if accepted else 0,
            completed=accepted,
            metadata={"candidate": "accepted" if accepted else "rejected"},
        )

    async def evaluate(self, _session):
        return EvaluationResult(
            success="flag{good}" in self.submissions,
            score=10 if "flag{good}" in self.submissions else 0,
        )


class ReactiveSession(FakeSession):
    def __init__(self, run_dir: Path) -> None:
        super().__init__(run_dir)
        self.negative_feedback = asyncio.Event()

    async def events(self):
        path = self.run_dir / "submission.inbox.jsonl"
        path.write_text(
            json.dumps({"type": "flag", "value": "flag{bad}"}) + "\n",
            encoding="utf-8",
        )
        yield AgentEvent(type="progress.updated", data={"step": 1, "made_progress": False})

        await asyncio.wait_for(self.negative_feedback.wait(), timeout=2)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps({"type": "flag", "value": "flag{good}"}) + "\n")
        yield AgentEvent(type="progress.updated", data={"step": 2, "made_progress": True})

    async def observe(self, observation):
        await super().observe(observation)
        if (
            observation.type == "benchmark.feedback"
            and observation.data.get("accepted") is False
        ):
            self.negative_feedback.set()


class ReactiveAgentAdapter(FakeAgentAdapter):
    async def start_session(self, *, run_dir: Path, **_kwargs):
        self.session = ReactiveSession(run_dir)
        return self.session


def test_benchmark_runner_replans_after_negative_feedback(monkeypatch, tmp_path: Path) -> None:
    import harness.benchmark.runner as runner_module

    fake_agent = ReactiveAgentAdapter()
    monkeypatch.setattr(
        runner_module,
        "build_agent_adapter",
        lambda *_args, **_kwargs: fake_agent,
    )
    benchmark = ReactiveBenchmarkAdapter()
    case = BenchmarkCase(id="CASE-REPLAN", benchmark="fake")
    agent = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "demo",
            "type": "cli",
            "command": ["true"],
        }
    )

    result = asyncio.run(
        BenchmarkRunner(runs_root=tmp_path).run_case(
            adapter=benchmark,
            case=case,
            agent=agent,
            budgets=BudgetSpec(wall_time=30),
            seed=1,
            submission_extractor=lambda _result: [],
            allow_host_agent=True,
        )
    )

    assert result["success"] is True
    assert benchmark.submissions == ["flag{bad}", "flag{good}"]
    assert fake_agent.session is not None
    feedback_types = [item.type for item in fake_agent.session.feedback]
    assert "solver.replan_requested" in feedback_types
    assert "solver.plan.updated" in feedback_types

    negative_feedback = next(
        item
        for item in fake_agent.session.feedback
        if item.type == "benchmark.feedback" and item.data["accepted"] is False
    )
    assert negative_feedback.data["replan_required"] is True
    assert "benchmark_negative_feedback" in negative_feedback.data["replan_reasons"]

    assert result["progress"]["rejected_submissions"] == 1
    assert result["progress"]["accepted_submissions"] == 1
    assert "benchmark_negative_feedback" not in result["progress"]["replan_reasons"]

    run_dir = next(tmp_path.glob("fake_CASE-REPLAN_*"))
    context = (run_dir / "world.context.txt").read_text(encoding="utf-8")
    assert "benchmark.candidate_rejected" in context
