from __future__ import annotations

import uuid

from pydantic import BaseModel, Field

from .progress import ProgressLedger
from .session import AgentEvent
from .world import Artifact, Capability, Failure, Hypothesis, Observation, WorldRepository


def _id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


class ActionIntent(BaseModel):
    schema_version: str = "harness/action-intent/v1"
    action_id: str = Field(default_factory=lambda: _id("action"))
    subgoal_id: str | None = None
    tool: str | None = None
    description: str = ""
    expected_observations: list[str] = Field(default_factory=list)
    success_conditions: list[str] = Field(default_factory=list)
    replan_conditions: list[str] = Field(default_factory=list)


class SemanticObservation(BaseModel):
    id: str | None = None
    action_id: str | None = None
    type: str = Field(min_length=1)
    summary: str = Field(min_length=1)
    content: dict = Field(default_factory=dict)
    confidence: float | None = Field(default=None, ge=0.0, le=1.0)
    source: str | None = None


class TypedACI:
    """Translate typed untrusted Agent events into authoritative semantic state."""

    @staticmethod
    def _require_agent_event(event: AgentEvent) -> None:
        if event.source != "agent":
            raise ValueError(f"{event.type} is only accepted from the agent event channel")

    @staticmethod
    def _require_runtime_event(event: AgentEvent) -> None:
        if event.source != "runtime" or not event.trusted:
            raise ValueError(f"{event.type} requires trusted runtime provenance")

    def apply(
        self,
        event: AgentEvent,
        *,
        world: WorldRepository,
        progress: ProgressLedger,
        actor: str,
    ) -> int:
        if event.type == "action.intent":
            self._require_agent_event(event)
            progress.record_intent(
                ActionIntent.model_validate(event.data),
                world_revision=world.snapshot.revision,
            )
            return 0

        if event.type == "world.observe":
            self._require_agent_event(event)
            item = SemanticObservation.model_validate(event.data)
            intent = progress.action_intent or {}
            current_action_id = intent.get("action_id")
            effective_action_id = item.action_id or (
                current_action_id if isinstance(current_action_id, str) else None
            )
            content = dict(item.content)
            if effective_action_id:
                content["action_id"] = effective_action_id
            progress.record_semantic_observation(
                summary=item.summary,
                action_id=effective_action_id,
            )
            world.upsert(
                "observation",
                Observation(
                    id=str(item.id or _id("obs")),
                    type=item.type,
                    content=content,
                    confidence=item.confidence,
                    source=item.source,
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type == "world.hypothesis":
            self._require_agent_event(event)
            data = event.data
            world.upsert(
                "hypothesis",
                Hypothesis(
                    id=str(data.get("id") or _id("hyp")),
                    statement=str(data["statement"]),
                    confidence=float(data.get("confidence", 0.5)),
                    status=str(data.get("status", "open")),
                    attributes=dict(data.get("attributes") or {}),
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type == "world.capability":
            self._require_agent_event(event)
            data = event.data
            world.upsert(
                "capability",
                Capability(
                    id=str(data.get("id") or _id("cap")),
                    type=str(data["type"]),
                    subject=data.get("subject"),
                    scope=data.get("scope"),
                    attributes=dict(data.get("attributes") or {}),
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type == "world.artifact":
            self._require_agent_event(event)
            data = event.data
            world.upsert(
                "artifact",
                Artifact(
                    id=str(data.get("id") or _id("artifact")),
                    type=str(data["type"]),
                    uri=data.get("uri"),
                    attributes=dict(data.get("attributes") or {}),
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type == "world.failure":
            self._require_agent_event(event)
            data = event.data
            world.upsert(
                "failure",
                Failure(
                    id=str(data.get("id") or _id("failure")),
                    type=str(data["type"]),
                    message=str(data["message"]),
                    action_id=data.get("action_id"),
                    recoverable=data.get("recoverable"),
                    attributes=dict(data.get("attributes") or {}),
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type in {"tool.call", "tool.result"}:
            self._require_runtime_event(event)
            # Raw execution telemetry belongs in trace/budget state, not semantic World state.
            return 0

        return 0
