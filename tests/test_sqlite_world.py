from __future__ import annotations

import json
import sqlite3
from concurrent.futures import ThreadPoolExecutor

import pytest

from redharness.world import (
    Goal,
    Observation,
    SQLiteWorldRepository,
    WorldConflictError,
    WorldEvent,
)


def test_sqlite_world_repository_is_transactional_and_reopenable(tmp_path) -> None:
    repository = SQLiteWorldRepository(tmp_path / "world.events.jsonl")

    repository.upsert(
        "observation",
        Observation(
            id="obs-1",
            type="network.service",
            content={"port": 443, "service": "https"},
        ),
        actor="agent:test",
    )
    repository.upsert(
        "goal",
        Goal(id="goal-1", description="finish task", status="active"),
    )

    assert repository.snapshot.revision == 2
    assert (tmp_path / "world.db").is_file()
    assert (tmp_path / "world.events.jsonl").is_file()
    assert (tmp_path / "world.snapshot.json").is_file()

    reopened = SQLiteWorldRepository(tmp_path / "world.db")
    assert reopened.snapshot == repository.snapshot

    with sqlite3.connect(tmp_path / "world.db") as connection:
        mode = connection.execute("PRAGMA journal_mode").fetchone()[0]
        assert mode.lower() == "wal"
        count = connection.execute("SELECT COUNT(*) FROM world_events").fetchone()[0]
        assert count == 2
        object_count = connection.execute(
            "SELECT COUNT(*) FROM world_objects"
        ).fetchone()[0]
        assert object_count == 2


def test_sqlite_world_repository_deduplicates_event_ids(tmp_path) -> None:
    repository = SQLiteWorldRepository(tmp_path / "world.db")
    event = WorldEvent(
        id="wevt-fixed",
        kind="goal",
        object={"id": "goal-1", "description": "demo"},
    )

    first = repository.append(event, expected_revision=0)
    second = repository.append(event, expected_revision=0)

    assert first.revision == 1
    assert second.revision == 1
    assert len(repository.events()) == 1

    conflicting = event.model_copy(
        update={"object": {"id": "goal-1", "description": "changed"}}
    )
    with pytest.raises(ValueError, match="different payload"):
        repository.append(conflicting)


def test_sqlite_world_repository_supports_expected_revision(tmp_path) -> None:
    path = tmp_path / "world.db"
    first = SQLiteWorldRepository(path)
    second = SQLiteWorldRepository(path)

    first.upsert(
        "goal",
        {"id": "goal-1", "description": "first"},
        expected_revision=0,
    )

    with pytest.raises(WorldConflictError, match="expected 0, current 1"):
        second.upsert(
            "goal",
            {"id": "goal-2", "description": "stale writer"},
            expected_revision=0,
        )

    second.upsert(
        "goal",
        {"id": "goal-2", "description": "fresh writer"},
        expected_revision=1,
    )
    assert first.snapshot.revision == 2
    assert set(first.snapshot.goals) == {"goal-1", "goal-2"}


def test_sqlite_world_repository_persists_sequence_cursor(tmp_path) -> None:
    repository = SQLiteWorldRepository(tmp_path / "world.db")
    for index in range(3):
        repository.upsert(
            "goal",
            {"id": f"goal-{index}", "description": f"goal {index}"},
        )

    sequenced = repository.sequenced_events(after_sequence=1)

    assert [sequence for sequence, _event in sequenced] == [2, 3]
    assert [event.object["id"] for _sequence, event in sequenced] == [
        "goal-1",
        "goal-2",
    ]
    assert [event.object["id"] for event in repository.events(after_sequence=2)] == [
        "goal-2"
    ]


def test_sqlite_world_repository_imports_legacy_jsonl(tmp_path) -> None:
    event_path = tmp_path / "world.events.jsonl"
    events = [
        WorldEvent(
            id="wevt-1",
            kind="goal",
            object={"id": "goal-1", "description": "legacy"},
        ),
        WorldEvent(
            id="wevt-2",
            kind="observation",
            object={
                "id": "obs-1",
                "type": "legacy.fact",
                "content": {"value": 1},
            },
        ),
    ]
    event_path.write_text(
        "".join(event.model_dump_json() + "\n" for event in events),
        encoding="utf-8",
    )

    repository = SQLiteWorldRepository(event_path)

    assert repository.snapshot.revision == 2
    assert repository.snapshot.goals["goal-1"].description == "legacy"
    assert repository.snapshot.observations["obs-1"].content["value"] == 1

    exported = [
        json.loads(line)
        for line in event_path.read_text(encoding="utf-8").splitlines()
        if line
    ]
    assert [item["id"] for item in exported] == ["wevt-1", "wevt-2"]



def test_sqlite_world_repository_serializes_concurrent_writers(tmp_path) -> None:
    path = tmp_path / "world.db"

    def write(index: int) -> None:
        repository = SQLiteWorldRepository(path)
        repository.upsert(
            "goal",
            {"id": f"goal-{index}", "description": f"goal {index}"},
        )

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(write, range(8)))

    repository = SQLiteWorldRepository(path)
    assert repository.snapshot.revision == 8
    assert len(repository.snapshot.goals) == 8
    assert [sequence for sequence, _event in repository.sequenced_events()] == list(
        range(1, 9)
    )
