from __future__ import annotations

import uuid
from datetime import UTC, datetime
from typing import Any, Literal, Self

from pydantic import BaseModel, Field, model_validator


def _id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


class WorldRecord(BaseModel):
    source: str | None = None
    observed_at: datetime | None = None
    valid_from: datetime | None = None
    expires_at: datetime | None = None
    supersedes: list[str] = Field(default_factory=list)

    @model_validator(mode="after")
    def validate_temporal_window(self) -> Self:
        if (
            self.valid_from is not None
            and self.expires_at is not None
            and self.expires_at < self.valid_from
        ):
            raise ValueError("expires_at must be greater than or equal to valid_from")
        return self


class Entity(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    attributes: dict[str, Any] = Field(default_factory=dict)


class Relation(WorldRecord):
    id: str
    source: str
    target: str
    type: str = Field(min_length=1)
    attributes: dict[str, Any] = Field(default_factory=dict)


class Observation(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    content: dict[str, Any] = Field(default_factory=dict)
    confidence: float | None = Field(default=None, ge=0.0, le=1.0)


class Artifact(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    uri: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Capability(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    subject: str | None = None
    scope: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Hypothesis(WorldRecord):
    id: str
    statement: str = Field(min_length=1)
    confidence: float = Field(ge=0.0, le=1.0)
    status: Literal["open", "supported", "refuted", "superseded"] = "open"
    attributes: dict[str, Any] = Field(default_factory=dict)


class Goal(WorldRecord):
    id: str
    description: str = Field(min_length=1)
    status: Literal["pending", "active", "completed", "failed", "abandoned"] = "pending"
    priority: float = Field(default=0.5, ge=0.0, le=1.0)
    parent_id: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class ActionRecord(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    status: Literal["planned", "running", "succeeded", "failed", "unknown"] = "unknown"
    target: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Constraint(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    description: str = Field(min_length=1)
    status: Literal["active", "satisfied", "violated", "expired"] = "active"
    scope: str | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


class Failure(WorldRecord):
    id: str
    type: str = Field(min_length=1)
    message: str = Field(min_length=1)
    action_id: str | None = None
    recoverable: bool | None = None
    attributes: dict[str, Any] = Field(default_factory=dict)


WorldObject = (
    Entity
    | Relation
    | Observation
    | Artifact
    | Capability
    | Hypothesis
    | Goal
    | ActionRecord
    | Constraint
    | Failure
)
WorldObjectKind = Literal[
    "entity",
    "relation",
    "observation",
    "artifact",
    "capability",
    "hypothesis",
    "goal",
    "action",
    "constraint",
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
    actions: dict[str, ActionRecord] = Field(default_factory=dict)
    constraints: dict[str, Constraint] = Field(default_factory=dict)
    failures: dict[str, Failure] = Field(default_factory=dict)
