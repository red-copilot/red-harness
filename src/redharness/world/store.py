from __future__ import annotations

import json
import threading
from pathlib import Path

from pydantic import BaseModel

from .models import WorldEvent, WorldObjectKind, WorldSnapshot
from .reducer import WorldReducer


class WorldStore:
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

    def replay(self) -> WorldSnapshot:
        if not self.event_path.exists():
            return WorldSnapshot()
        events: list[WorldEvent] = []
        with self.event_path.open("r", encoding="utf-8") as handle:
            for number, line in enumerate(handle, start=1):
                if not line.strip():
                    continue
                try:
                    events.append(WorldEvent.model_validate_json(line))
                except Exception as exc:  # noqa: BLE001 - persisted state boundary.
                    raise ValueError(
                        f"invalid world event at {self.event_path}:{number}: {exc}"
                    ) from exc
        return self._reducer.replay(events)

    def append(self, event: WorldEvent) -> WorldSnapshot:
        line = event.model_dump_json()
        with self._lock:
            with self.event_path.open("a", encoding="utf-8") as handle:
                handle.write(line + "\n")
                handle.flush()
            self._reducer.apply(self._snapshot, event)
            self._write_snapshot()
            return self.snapshot

    def upsert(
        self,
        kind: WorldObjectKind,
        obj: BaseModel | dict,
        *,
        actor: str = "harness",
        source_event_id: str | None = None,
    ) -> WorldSnapshot:
        data = obj.model_dump(mode="json") if isinstance(obj, BaseModel) else dict(obj)
        return self.append(
            WorldEvent(
                kind=kind,
                op="upsert",
                object=data,
                actor=actor,
                source_event_id=source_event_id,
            )
        )

    def remove(
        self,
        kind: WorldObjectKind,
        object_id: str,
        *,
        actor: str = "harness",
    ) -> WorldSnapshot:
        return self.append(
            WorldEvent(kind=kind, op="remove", object={"id": object_id}, actor=actor)
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
