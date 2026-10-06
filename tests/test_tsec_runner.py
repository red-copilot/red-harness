import asyncio
import json
from pathlib import Path
from types import SimpleNamespace

from redharness.agent import AgentResult
from redharness.benchmark import tsec
from redharness.budget import UsageMetrics
from redharness.models import AgentSpec, BudgetSpec


class FakeAdapter:
    def run(self, *args, **kwargs):
        del args, kwargs
        records = [
            {
                "type": "message_end",
                "message": {
                    "role": "assistant",
                    "content": [
                        {
                            "type": "text",
                            "text": "REDHARNESS_FLAG=flag{one} REDHARNESS_FLAG=flag{two}",
                        }
                    ],
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
            cumulative_score=count * 50,
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
    )
    agent = AgentSpec.model_validate(
        {
            "apiVersion": "redharness/v1",
            "id": "pi-tsec",
            "type": "pi",
            "image": "pi:test",
            "network": "host",
            "pi": {"provider": "fake", "model": "m"},
        }
    )
    client = FakeClient()

    result = asyncio.run(
        tsec.TSecRunner(runs_root=tmp_path).run_challenge(
            client=client,
            challenge=challenge,
            agent=agent,
            budgets=BudgetSpec(wall_time=30),
            seed=1,
        )
    )

    assert result["success"] is True
    assert result["score"] == 100
    assert result["flags"] == {"expected": 2, "correct": 2}
    assert client.closed is True
    assert [flag for _, flag in client.submitted] == ["flag{one}", "flag{two}"]

    run_dir = next(tmp_path.iterdir())
    trace = (run_dir / "trace.jsonl").read_text(encoding="utf-8")
    assert "flag{one}" not in trace
    assert "benchmark.challenge.closed" in trace
    assert "benchmark.submission" in trace
