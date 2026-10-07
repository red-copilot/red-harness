import asyncio
from pathlib import Path
from types import SimpleNamespace

from redharness.benchmark import tsec
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


class FakeClient:
    def __init__(self, *_args, **_kwargs) -> None:
        self.closed = False

    async def __aenter__(self):
        return self

    async def __aexit__(self, *_args) -> None:
        return None

    async def list_challenges(self):
        return [
            SimpleNamespace(
                unique_code="WEB-001",
                description="Authorized web challenge",
                category="web",
                flag_count=2,
                correct_flag_count=0,
                difficulty="medium",
                level="L2",
                total_score=100,
                is_completed=False,
            )
        ]

    async def start_challenge(self, code):
        assert code == "WEB-001"
        return SimpleNamespace(container_addr=["10.0.0.2:8080"])

    async def close_challenge(self, code):
        assert code == "WEB-001"
        self.closed = True
        return SimpleNamespace(closed=True)


class FakeBenchmarkRunner:
    last_budgets: BudgetSpec | None = None

    def __init__(self, *, runs_root: Path):
        self.runs_root = runs_root

    async def run_case(self, *, case, budgets, **_kwargs):
        type(self).last_budgets = budgets
        return {
            "run_id": "tsec_WEB-001_fake",
            "benchmark": {
                "type": "tsec",
                "case_id": case.id,
                "domain": case.domain,
                "difficulty": case.difficulty,
                "session": {
                    "hint_used": False,
                    "initial_correct": 0,
                    "total_flags": 2,
                    "remaining_at_start": 2,
                },
            },
            "status": "finished",
            "success": True,
            "score": 100.0,
            "evaluation": {
                "expected_flags": 2,
                "initial_correct": 0,
                "remaining_at_start": 2,
                "correct": 2,
                "platform_cumulative_score": 900,
            },
        }


def test_tsec_runner_uses_generic_benchmark_contract(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr(tsec, "TSecClientAdapter", FakeClient)
    monkeypatch.setattr(tsec, "BenchmarkRunner", FakeBenchmarkRunner)

    results = asyncio.run(
        tsec.TSecRunner(runs_root=tmp_path).run(
            config=tsec.TSecConfig(base_url="https://example.invalid", token="secret"),
            agent=_agent(),
            budgets=BudgetSpec(wall_time=30),
            challenge_code="WEB-001",
            seed=1,
        )
    )

    assert len(results) == 1
    result = results[0]
    assert result["success"] is True
    assert result["score"] == 100.0
    assert result["platform_cumulative_score"] == 900
    assert result["flags"] == {
        "expected": 2,
        "initial_correct": 0,
        "remaining_at_start": 2,
        "correct": 2,
    }
    assert result["benchmark"]["challenge"] == "WEB-001"
    assert result["benchmark"]["level"] == "L2"


def test_tsec_runner_applies_platform_wall_time_ceiling(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr(tsec, "TSecClientAdapter", FakeClient)
    monkeypatch.setattr(tsec, "BenchmarkRunner", FakeBenchmarkRunner)

    asyncio.run(
        tsec.TSecRunner(runs_root=tmp_path).run(
            config=tsec.TSecConfig(base_url="https://example.invalid", token="secret"),
            agent=_agent(),
            budgets=BudgetSpec(wall_time=7200),
            challenge_code="WEB-001",
        )
    )

    assert FakeBenchmarkRunner.last_budgets is not None
    assert FakeBenchmarkRunner.last_budgets.wall_time == 3600
