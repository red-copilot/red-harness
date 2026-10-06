from __future__ import annotations

from types import SimpleNamespace

import pytest

from redharness.benchmark.base import BenchmarkCase, Submission
from redharness.benchmark.tsec_adapter import TSecBenchmarkAdapter


class FakeTSecClient:
    def __init__(self) -> None:
        self.closed: list[str] = []
        self.submitted: list[tuple[str, str]] = []

    async def list_challenges(self):
        return [
            SimpleNamespace(
                unique_code="WEB-001",
                description="Find the flag",
                category="web",
                difficulty="easy",
                level="L1",
                flag_count=1,
                correct_flag_count=0,
                total_score=100,
                is_completed=False,
            )
        ]

    async def start_challenge(self, unique_code: str):
        assert unique_code == "WEB-001"
        return SimpleNamespace(container_addr=["10.0.0.10:8080"])

    async def get_hint(self, unique_code: str):
        return SimpleNamespace(hint="look at the service")

    async def submit_flag(self, unique_code: str, flag: str):
        self.submitted.append((unique_code, flag))
        return SimpleNamespace(
            correct=True,
            awarded=100,
            cumulative_score=100,
            correct_flag_count=1,
            total_flag_count=1,
            matched_flag_index=0,
        )

    async def close_challenge(self, unique_code: str):
        self.closed.append(unique_code)
        return SimpleNamespace(closed=True)


@pytest.mark.asyncio
async def test_tsec_adapter_maps_sdk_to_generic_contracts() -> None:
    client = FakeTSecClient()
    adapter = TSecBenchmarkAdapter(client, use_hint=True)

    cases = await adapter.discover()
    assert cases == [
        BenchmarkCase(
            id="WEB-001",
            benchmark="tsec",
            domain="web",
            difficulty="easy",
            metadata={
                "level": "L1",
                "flag_count": 1,
                "correct_flag_count": 0,
                "total_score": 100,
                "is_completed": False,
            },
        )
    ]

    session = await adapter.provision(cases[0])
    assert session.case_id == "WEB-001"
    assert session.targets[0].address == "10.0.0.10:8080"
    assert session.metadata["hint_used"] is True
    assert "look at the service" in session.objective.description

    submitted = await adapter.submit(
        session,
        Submission(type="flag", value="flag{ok}"),
    )
    assert submitted.accepted is True
    assert submitted.completed is True
    assert submitted.score_delta == 100

    evaluation = await adapter.evaluate(session)
    assert evaluation.success is True
    assert evaluation.score == 100
    assert evaluation.metadata["correct"] == 1

    await adapter.teardown(session)
    assert client.closed == ["WEB-001"]


@pytest.mark.asyncio
async def test_tsec_adapter_rejects_non_flag_submission() -> None:
    client = FakeTSecClient()
    adapter = TSecBenchmarkAdapter(client)
    case = (await adapter.discover())[0]
    session = await adapter.provision(case)

    result = await adapter.submit(
        session,
        Submission(type="finding", value="not-a-flag"),
    )
    assert result.accepted is False
    assert result.metadata["reason"] == "unsupported_submission_type"

    await adapter.teardown(session)
