from __future__ import annotations

from typing import Protocol

from pydantic import BaseModel

from .models import WorldEvent, WorldObjectKind, WorldSnapshot


class WorldRepository(Protocol):
    @property
    def snapshot(self) -> WorldSnapshot: ...

    def events(self, *, limit: int | None = None) -> list[WorldEvent]: ...

    def replay(self) -> WorldSnapshot: ...

    def append(self, event: WorldEvent) -> WorldSnapshot: ...

    def upsert(
        self,
        kind: WorldObjectKind,
        obj: BaseModel | dict,
        *,
        actor: str = "harness",
        source_event_id: str | None = None,
    ) -> WorldSnapshot: ...

    def remove(
        self,
        kind: WorldObjectKind,
        object_id: str,
        *,
        actor: str = "harness",
    ) -> WorldSnapshot: ...
