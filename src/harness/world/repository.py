from __future__ import annotations

from typing import Protocol

from pydantic import BaseModel

from .models import WorldEvent, WorldObjectKind, WorldSnapshot


class WorldConflictError(RuntimeError):
    pass


class WorldRepository(Protocol):
    @property
    def snapshot(self) -> WorldSnapshot: ...

    def events(
        self,
        *,
        limit: int | None = None,
        after_sequence: int | None = None,
    ) -> list[WorldEvent]: ...

    def replay(self) -> WorldSnapshot: ...

    def flush(self) -> None: ...

    def append(
        self,
        event: WorldEvent,
        *,
        expected_revision: int | None = None,
    ) -> WorldSnapshot: ...

    def upsert(
        self,
        kind: WorldObjectKind,
        obj: BaseModel | dict,
        *,
        actor: str = "harness",
        source_event_id: str | None = None,
        expected_revision: int | None = None,
    ) -> WorldSnapshot: ...

    def remove(
        self,
        kind: WorldObjectKind,
        object_id: str,
        *,
        actor: str = "harness",
        expected_revision: int | None = None,
    ) -> WorldSnapshot: ...
