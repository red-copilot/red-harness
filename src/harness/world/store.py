from __future__ import annotations

import json
import threading
from pathlib import Path

from pydantic import BaseModel

from .models import WorldEvent, WorldObjectKind, WorldSnapshot
from .reducer import WorldReducer
from .repository import WorldConflictError


class FileWorldRepository:
    """Append-only world event store with a materialized JSON snapshot."""

    def __init__(self, event_path: Path, snapshot_path: Path | None = None) -> None:
        self.event_path = event_path
        self.snapshot_path = snapshot_path or event_path.with_name("world.snapshot.json")
        self.event_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._reducer = WorldReducer()
        self._snapshot = self.replay()

    @property
    def snapshot(self) -> WorldSnapshot:
        return self._snapshot.model_copy(deep=True)

    def events(
        self,
        *,
        limit: int | None = None,
        after_sequence: int | None = None,
    ) -> list[WorldEvent]:
        if not self.event_path.exists():
            return []
        events: list[WorldEvent] = []
        sequence = 0
        with self.event_path.open("r", encoding="utf-8") as handle:
            for number, line in enumerate(handle, start=1):
                if not line.strip():
                    continue
                sequence += 1
                if after_sequence is not None and sequence <= after_sequence:
                    continue
                try:
                    events.append(WorldEvent.model_validate_json(line))
                except Exception as exc:
                    raise ValueError(
                        f"invalid world event at {self.event_path}:{number}: {exc}"
                    ) from exc
                if limit is not None and len(events) >= limit:
                    break
        return events

    def replay(self) -> WorldSnapshot:
        return self._reducer.replay(self.events())

    def flush(self) -> None:
        """Persist the current materialized snapshot; file events are already durable."""
        self._write_snapshot()

    def append(
        self,
        event: WorldEvent,
        *,
        expected_revision: int | None = None,
    ) -> WorldSnapshot:
        line = event.model_dump_json()
        with self._lock:
            for existing in self.events():
                if existing.id == event.id:
                    if existing.model_dump_json() != line:
                        raise ValueError(
                            f"world event id {event.id} already exists with different payload"
                        )
                    return self.snapshot
            if (
                expected_revision is not None
                and expected_revision != self._snapshot.revision
            ):
                raise WorldConflictError(
                    f"world revision conflict: expected {expected_revision}, "
                    f"current {self._snapshot.revision}"
                )
            candidate = self._snapshot.model_copy(deep=True)
            self._reducer.apply(candidate, event)
            with self.event_path.open("a", encoding="utf-8") as handle:
                handle.write(line + "\n")
                handle.flush()
            self._snapshot = candidate
            self._write_snapshot()
            return self.snapshot

    def upsert(
        self,
        kind: WorldObjectKind,
        obj: BaseModel | dict,
        *,
        actor: str = "harness",
        source_event_id: str | None = None,
        expected_revision: int | None = None,
    ) -> WorldSnapshot:
        data = obj.model_dump(mode="json") if isinstance(obj, BaseModel) else dict(obj)
        return self.append(
            WorldEvent(
                kind=kind,
                op="upsert",
                object=data,
                actor=actor,
                source_event_id=source_event_id,
            ),
            expected_revision=expected_revision,
        )

    def remove(
        self,
        kind: WorldObjectKind,
        object_id: str,
        *,
        actor: str = "harness",
        expected_revision: int | None = None,
    ) -> WorldSnapshot:
        return self.append(
            WorldEvent(kind=kind, op="remove", object={"id": object_id}, actor=actor),
            expected_revision=expected_revision,
        )

    def _write_snapshot(self) -> None:
        temp = self.snapshot_path.with_suffix(self.snapshot_path.suffix + ".tmp")
        temp.write_text(
            json.dumps(
                self._snapshot.model_dump(mode="json"),
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
        temp.replace(self.snapshot_path)


WorldStore = FileWorldRepository
