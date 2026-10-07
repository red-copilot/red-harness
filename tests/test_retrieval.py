from __future__ import annotations

from datetime import UTC, datetime, timedelta

from harness.world import (
    Goal,
    Observation,
    WorldContextBuilder,
    WorldRetriever,
    WorldSnapshot,
)


def test_world_retriever_prefers_goal_relevant_observation() -> None:
    now = datetime.now(UTC)
    snapshot = WorldSnapshot(
        goals={
            "goal-1": Goal(
                id="goal-1",
                description="Find the web admin endpoint",
                status="active",
                priority=1.0,
            )
        },
        observations={
            "obs-a": Observation(
                id="obs-a",
                type="network.banner",
                content={"service": "ssh", "port": 22},
                observed_at=now,
            ),
            "obs-b": Observation(
                id="obs-b",
                type="web.endpoint",
                content={"path": "/admin", "status": 200},
                observed_at=now - timedelta(seconds=1),
            ),
        },
    )
    retriever = WorldRetriever(limits={"observations": 1})

    selected = retriever.retrieve(snapshot)

    assert [item.id for item in selected["observations"]] == ["obs-b"]


def test_world_context_is_bounded_and_filters_expired_state() -> None:
    now = datetime.now(UTC)
    snapshot = WorldSnapshot(
        goals={
            "goal-1": Goal(
                id="goal-1",
                description="Find target service",
                status="active",
                priority=1.0,
            )
        },
        observations={
            "live": Observation(
                id="live",
                type="network.service",
                content={"service": "http", "detail": "x" * 200},
                valid_from=now - timedelta(minutes=1),
                expires_at=now + timedelta(minutes=5),
            ),
            "expired": Observation(
                id="expired",
                type="network.service",
                content={"service": "old"},
                valid_from=now - timedelta(minutes=10),
                expires_at=now - timedelta(minutes=1),
            ),
        },
    )
    context = WorldContextBuilder(max_chars=350).render(
        snapshot,
        query="target http service",
    )

    assert "live" in context
    assert "expired" not in context
    assert len(context) <= 350
    assert "context truncated" in context
