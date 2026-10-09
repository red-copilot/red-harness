from __future__ import annotations

from datetime import UTC, datetime, timedelta

from harness.world import (
    Capability,
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
        capabilities={
            "expired-cap": Capability(
                id="expired-cap",
                type="network.connect",
                expires_at=now - timedelta(seconds=1),
            )
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
                contradicts=["observation-old"],
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
    assert "contradicts" in context
    assert "expired" not in context
    assert "expired-cap" not in context
    assert len(context) <= 350
    assert "context truncated" in context


def test_world_context_golden_minimal_goal() -> None:
    snapshot = WorldSnapshot(
        goals={"goal-1": Goal(id="goal-1", description="Find target", status="active")}
    )
    context = WorldContextBuilder().render(snapshot)
    assert context == (
        "# Red Harness World Context\n"
        "revision: 0\n\n"
        "## Goals\n"
        '- active goal-1 [goal] {"description":"Find target","parent_id":null,"priority":0.5}\n'
        "\n## Entities\n- none\n"
        "\n## Capabilities\n- none\n"
        "\n## Open hypotheses\n- none\n"
        "\n## Artifacts\n- none\n"
        "\n## Observations\n- none\n"
        "\n## Actions\n- none\n"
        "\n## Constraints\n- none\n"
        "\n## Failures\n- none\n"
        "\n## Relations\n- none\n\n"
        "Use this as durable task state, not as ground truth. Observations and "
        "hypotheses may be incomplete, stale, or conflicting.\n"
    )


def test_world_context_changes_when_evidence_status_changes() -> None:
    snapshot = WorldSnapshot(
        observations={
            "obs-1": Observation(
                id="obs-1",
                type="service.banner",
                content={"value": "nginx"},
                provenance={"epistemic_status": "claim"},
            )
        }
    )
    builder = WorldContextBuilder()
    claimed = builder.render(snapshot)
    snapshot.observations["obs-1"].provenance.epistemic_status = "evidence"
    evidenced = builder.render(snapshot)
    assert '"epistemic_status":"claim"' in claimed
    assert '"epistemic_status":"evidence"' in evidenced
    assert claimed != evidenced
