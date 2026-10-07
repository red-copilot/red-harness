from __future__ import annotations

import json

from redharness.world import WorldStore
from redharness.world.live import WorldInboxCursor
from redharness.world.protocol import WorldSubmission, write_submission


def test_world_inbox_cursor_ingests_only_new_lines(tmp_path) -> None:
    store = WorldStore(tmp_path / "world.events.jsonl")
    inbox = tmp_path / "world.inbox.jsonl"
    cursor = WorldInboxCursor(inbox, store, actor="agent:test")

    write_submission(
        inbox,
        WorldSubmission(
            kind="observation",
            object={
                "id": "obs-1",
                "type": "network.service",
                "content": {"port": 443},
            },
        ),
    )

    first = cursor.poll()
    assert first.accepted == 1
    assert first.rejected == 0
    assert store.snapshot.revision == 1

    second = cursor.poll()
    assert second.accepted == 0
    assert second.rejected == 0
    assert store.snapshot.revision == 1

    write_submission(
        inbox,
        WorldSubmission(
            kind="capability",
            object={
                "id": "cap-1",
                "type": "network.reachability",
                "subject": "agent",
                "scope": "target",
            },
        ),
    )

    third = cursor.poll()
    assert third.accepted == 1
    assert store.snapshot.revision == 2


def test_world_inbox_cursor_handles_partial_and_invalid_lines(tmp_path) -> None:
    store = WorldStore(tmp_path / "world.events.jsonl")
    inbox = tmp_path / "world.inbox.jsonl"
    cursor = WorldInboxCursor(inbox, store, actor="agent:test")

    with inbox.open("w", encoding="utf-8") as handle:
        handle.write(
            json.dumps(
                {
                    "schema_version": "redharness.world.submit/v1",
                    "kind": "observation",
                    "object": {
                        "id": "obs-partial",
                        "type": "network.banner",
                        "content": {"value": "http"},
                    },
                }
            )
        )

    pending = cursor.poll()
    assert pending.accepted == 0
    assert store.snapshot.revision == 0

    with inbox.open("a", encoding="utf-8") as handle:
        handle.write("\n")
        handle.write('{"kind":"unknown","object":{"id":"bad"}}\n')

    finished = cursor.poll()
    assert finished.accepted == 1
    assert finished.rejected == 1
    assert store.snapshot.revision == 1
    assert "obs-partial" in store.snapshot.observations
