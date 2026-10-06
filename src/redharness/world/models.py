from __future__ import annotations

import uuid
from datetime import UTC, datetime
from typing import Any, Literal

from pydantic import BaseModel, Field


def _id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


class Entity(BaseModel):
    id: str
    type: str = Field(min_length=1)
    attributes: dict[str, Any] = Field(default_factory=dict)


class Relation(BaseModel):
    id: str
    source: str
    target: str
    type: str = Field(min_length=1)
    attributes: dict[str, Any] = Field(default_factory=dict)


class Observation(BaseModel):
    id: str
    type: str = Field(min_length=1)
    content: dict[str, Any] = Field(default_factory=dict)
    confidence: float | None = Field(default=None, ge=0.0, le=1.0)
    source: str | None = None


class Artifact(BaseModel):
    id: str
    type: str = Field(min_length=1)
    uri: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Capability(BaseModel):
    id: str
    type: str = Field(min_length=1)
    subject: str | None = None
    scope: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Hypothesis(BaseModel):
    id: str
    statement: str = Field(min_length=1)
    confidence: float = Field(ge=0.0, le=1.0)
    status: Literal["open", "supported", "refuted", "superseded"] = "open"
    attributes: dict[str, Any] = Field(default_factory=dict)


class Goal(BaseModel):
    id: str
    description: str = Field(min_length=1)
    status: Literal["pending", "active", "completed", "failed", "abandoned"] = "pending"
    priority: float = Field(default=0.5, ge=0.0, le=1.0)
    parent_id: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Failure(BaseModel):
    id: str
    type: str = Field(min_length=1)
    message: str = Field(min_length=1)
    action_id: str | None = None
    recoverable: bool | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


WorldObject = Entity | Relation | Observation | Artifact | Capability | Hypothesis | Goal | Failure
WorldObjectKind = Literal[
    "entity",
    "relation",
    "observation",
    "artifact",
    "capability",
    "hypothesis",
    "goal",
    "failure",
]


class WorldEvent(BaseModel):
    schema_version: Literal["redharness.world/v1"] = "redharness.world/v1"
    id: str = Field(default_factory=lambda: _id("wevt"))
    ts: datetime = Field(default_factory=lambda: datetime.now(UTC))
    kind: WorldObjectKind
    op: Literal["upsert", "remove"] = "upsert"
    object: dict[str, Any]
    actor: str = "harness"
    source_event_id: str | None = None


class WorldSnapshot(BaseModel):
    schema_version: Literal["redharness.world/v1"] = "redharness.world/v1"
    revision: int = 0
    entities: dict[str, Entity] = Field(default_factory=dict)
    relations: dict[str, Relation] = Field(default_factory=dict)
    observations: dict[str, Observation] = Field(default_factory=dict)
    artifacts: dict[str, Artifact] = Field(default_factory=dict)
    capabilities: dict[str, Capability] = Field(default_factory=dict)
    hypotheses: dict[str, Hypothesis] = Field(default_factory=dict)
    goals: dict[str, Goal] = Field(default_factory=dict)
    failures: dict[str, Failure] = Field(default_factory=dict)
