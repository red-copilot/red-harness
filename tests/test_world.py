from __future__ import annotations

import json

from redharness.world import (
    Capability,
    Goal,
    Hypothesis,
    Observation,
    Relation,
    WorldStore,
)


def test_world_store_replays_and_materializes_snapshot(tmp_path):
    event_path = tmp_path / "world.events.jsonl"
    store = WorldStore(event_path)

    store.upsert(
        "observation",
        Observation(
            id="obs-1",
            type="network.port",
            content={"host": "target", "port": 443, "state": "open"},
            confidence=0.98,
            source="scanner",
        ),
        actor="agent",
    )
    store.upsert(
        "capability",
        Capability(
            id="cap-1",
            type="network.tcp_connect",
            subject="agent",
            scope="target:443",
        ),
    )
    store.upsert(
        "hypothesis",
        Hypothesis(
            id="hyp-1",
            statement="target exposes an HTTPS service",
            confidence=0.8,
        ),
    )
    store.upsert(
        "relation",
        Relation(
            id="rel-1",
            source="obs-1",
            target="hyp-1",
            type="supports",
        ),
    )

    snapshot = store.snapshot
    assert snapshot.revision == 4
    assert snapshot.observations["obs-1"].content["port"] == 443
    assert snapshot.capabilities["cap-1"].scope == "target:443"
    assert snapshot.relations["rel-1"].type == "supports"

    restored = WorldStore(event_path)
    assert restored.snapshot == snapshot

    materialized = json.loads((tmp_path / "world.snapshot.json").read_text())
    assert materialized["revision"] == 4
    assert materialized["hypotheses"]["hyp-1"]["status"] == "open"


def test_world_store_supports_updates_and_removals(tmp_path):
    store = WorldStore(tmp_path / "world.events.jsonl")
    goal = Goal(id="goal-1", description="obtain objective", status="active", priority=1.0)

    store.upsert("goal", goal)
    goal.status = "completed"
    store.upsert("goal", goal)
    store.remove("goal", "goal-1")

    assert store.snapshot.revision == 3
    assert "goal-1" not in store.snapshot.goals
