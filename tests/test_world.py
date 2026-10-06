from __future__ import annotations

import json

from redharness.world import (
    Capability,
    Goal,
    Hypothesis,
    Observation,
    Relation,
    WorldStore,
    WorldSubmission,
    ingest_world_inbox,
    write_submission,
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



def test_world_inbox_validates_agent_submissions(tmp_path):
    store = WorldStore(tmp_path / "world.events.jsonl")
    inbox = tmp_path / "world.inbox.jsonl"

    write_submission(
        inbox,
        WorldSubmission(
            kind="capability",
            object={
                "id": "cap-shell",
                "type": "host.shell",
                "subject": "agent",
                "scope": "host-1",
                "attributes": {},
            },
        ),
    )
    with inbox.open("a", encoding="utf-8") as handle:
        handle.write('{"kind":"capability","object":{"type":"missing-id"}}\n')
        handle.write('{"kind":"unknown","object":{"id":"bad"}}\n')

    report = ingest_world_inbox(inbox, store, actor="agent:test")

    assert report.accepted == 1
    assert report.rejected == 2
    assert store.snapshot.capabilities["cap-shell"].scope == "host-1"

    persisted = [
        json.loads(line)
        for line in (tmp_path / "world.events.jsonl").read_text().splitlines()
        if line
    ]
    assert persisted[0]["actor"] == "agent:test"
