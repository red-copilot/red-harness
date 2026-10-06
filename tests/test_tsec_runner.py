import asyncio
import json
from pathlib import Path
from types import SimpleNamespace

from redharness.agent import AgentResult
from redharness.benchmark import tsec
from redharness.budget import UsageMetrics
from redharness.models import AgentSpec, BudgetSpec


def _agent() -> AgentSpec:
    return AgentSpec.model_validate(
        {
            "apiVersion": "redharness/v1",
            "id": "pi-tsec",
            "type": "pi",
            "image": "pi:test",
            "network": "host",
            "pi": {"provider": "fake", "model": "m"},
        }
    )


class FakeAdapter:
    def __init__(self, flags: list[str] | None = None):
        self.flags = flags or ["flag{one}", "flag{two}"]

    def run(self, *args, **kwargs):
        del args, kwargs
        text = " ".join(f"REDHARNESS_FLAG={flag}" for flag in self.flags)
        records = [
            {
                "type": "message_end",
                "message": {
                    "role": "assistant",
                    "content": [{"type": "text", "text": text}],
                },
            }
        ]
        return AgentResult(
            returncode=0,
            timed_out=False,
            budget_exceeded=None,
            stdout="\n".join(json.dumps(item) for item in records),
            stderr="",
            metrics=UsageMetrics(tool_calls=3, total_tokens=10),
        )


class FakeClient:
    def __init__(self):
        self.closed = False
        self.submitted = []

    async def start_challenge(self, code):
        assert code == "WEB-001"
        return SimpleNamespace(container_addr=["10.0.0.2:8080"])

    async def submit_flag(self, code, flag):
        self.submitted.append((code, flag))
        count = len(self.submitted)
        return SimpleNamespace(
            correct=True,
            awarded=50,
            cumulative_score=800 + count * 50,
            correct_flag_count=count,
            total_flag_count=2,
            matched_flag_index=count - 1,
        )

    async def close_challenge(self, code):
        assert code == "WEB-001"
        self.closed = True
        return SimpleNamespace(closed=True)


def test_tsec_runner_submits_flags_and_closes(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr(tsec, "build_agent_adapter", lambda *args, **kwargs: FakeAdapter())
    challenge = SimpleNamespace(
        unique_code="WEB-001",
        description="Authorized web challenge",
        flag_count=2,
        correct_flag_count=0,
        difficulty="medium",
        level="L2",
        total_score=100,
    )
    client = FakeClient()

    result = asyncio.run(
        tsec.TSecRunner(runs_root=tmp_path).run_challenge(
            client=client,
            challenge=challenge,
            agent=_agent(),
            budgets=BudgetSpec(wall_time=30),
            seed=1,
        )
    )

    assert result["success"] is True
    assert result["score"] == 100
    assert result["platform_cumulative_score"] == 900
    assert result["flags"] == {
        "expected": 2,
        "initial_correct": 0,
        "remaining_at_start": 2,
        "correct": 2,
    }
    assert result["benchmark"]["challenge_total_score"] == 100
    assert client.closed is True
    assert [flag for _, flag in client.submitted] == ["flag{one}", "flag{two}"]

    run_dir = next(tmp_path.iterdir())
    trace = (run_dir / "trace.jsonl").read_text(encoding="utf-8")
    assert "flag{one}" not in trace
    assert "benchmark.challenge.closed" in trace
    assert "benchmark.submission" in trace


class PartialProgressClient:
    def __init__(self):
        self.hint_calls = 0
        self.closed = False

    async def start_challenge(self, _code):
        return SimpleNamespace(container_addr=["10.0.0.9:443"])

    async def get_hint(self, _code):
        self.hint_calls += 1
        return SimpleNamespace(hint="Inspect the alternate endpoint")

    async def submit_flag(self, _code, _flag):
        return SimpleNamespace(
            correct=True,
            awarded=35,
            cumulative_score=935,
            correct_flag_count=2,
            total_flag_count=2,
            matched_flag_index=1,
        )

    async def close_challenge(self, _code):
        self.closed = True
        return SimpleNamespace(closed=True)


def test_tsec_progress_hint_and_score_semantics(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr(
        tsec,
        "build_agent_adapter",
        lambda *args, **kwargs: FakeAdapter(flags=["flag{new}"]),
    )
    challenge = SimpleNamespace(
        unique_code="WEB-002",
        description="Partially completed challenge",
        flag_count=2,
        correct_flag_count=1,
        difficulty="hard",
        level="L3",
        total_score=120,
    )
    client = PartialProgressClient()

    result = asyncio.run(
        tsec.TSecRunner(runs_root=tmp_path).run_challenge(
            client=client,
            challenge=challenge,
            agent=_agent(),
            budgets=BudgetSpec(wall_time=30),
            seed=2,
            use_hint=True,
        )
    )

    assert result["success"] is True
    assert result["score"] == 35
    assert result["platform_cumulative_score"] == 935
    assert result["flags"]["initial_correct"] == 1
    assert result["flags"]["remaining_at_start"] == 1
    assert result["flags"]["correct"] == 2
    assert result["benchmark"]["hint_used"] is True
    assert client.hint_calls == 1
    assert client.closed is True


class ResourceUnavailable(Exception):
    pass


class RetryClient:
    def __init__(self):
        self.starts = 0

    async def start_challenge(self, _code):
        self.starts += 1
        if self.starts < 3:
            raise ResourceUnavailable("busy")
        return SimpleNamespace(container_addr=["10.0.0.3:80"])


def test_tsec_start_retries_resource_unavailable(tmp_path: Path) -> None:
    trace = tsec.TraceRecorder(tmp_path / "trace.jsonl", "tsec_retry", "WEB-003")
    client = RetryClient()
    started = asyncio.run(
        tsec.TSecRunner(runs_root=tmp_path)._start_challenge(
            client=client,
            challenge_code="WEB-003",
            trace=trace,
            retries=2,
            retry_delay=0,
        )
    )
    assert started.container_addr == ["10.0.0.3:80"]
    assert client.starts == 3
    assert "benchmark.challenge.start_error" in (
        tmp_path / "trace.jsonl"
    ).read_text(encoding="utf-8")
