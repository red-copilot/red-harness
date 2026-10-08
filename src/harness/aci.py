from __future__ import annotations

import uuid

from pydantic import BaseModel, Field

from .progress import ProgressLedger
from .session import AgentEvent
from .world import (
    ActionRecord,
    Artifact,
    Capability,
    Failure,
    Hypothesis,
    Observation,
    Provenance,
    WorldRepository,
)


def _id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


class ActionIntent(BaseModel):
    schema_version: str = "harness/action-intent/v1"
    action_id: str = Field(default_factory=lambda: _id("action"))
    subgoal_id: str | None = None
    skill_id: str | None = None
    tool: str | None = None
    description: str = ""
    expected_observations: list[str] = Field(default_factory=list)
    success_conditions: list[str] = Field(default_factory=list)
    replan_conditions: list[str] = Field(default_factory=list)


class TypedACI:
    """Translate small typed Agent events into authoritative solver/world state."""

    def apply(
        self,
        event: AgentEvent,
        *,
        world: WorldRepository,
        progress: ProgressLedger,
        actor: str,
    ) -> int:
        if event.type == "action.intent":
            progress.record_intent(ActionIntent.model_validate(event.data))
            return 0

        if event.type == "world.observe":
            data = event.data
            summary = data.get("summary")
            if isinstance(summary, str) and summary:
                progress.actual_observation = summary
            world.upsert(
                "observation",
                Observation(
                    id=str(data.get("id") or _id("obs")),
                    type=str(data["type"]),
                    content=dict(data.get("content") or {}),
                    confidence=data.get("confidence"),
                    source=data.get("source"),
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type == "world.hypothesis":
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

        if event.type == "tool.call":
            call_id = str(event.data.get("tool_call_id") or _id("tool"))
            intent = progress.action_intent
            world.upsert(
                "action",
                ActionRecord(
                    id=f"action:{call_id}",
                    type=str(event.data.get("tool") or "tool"),
                    status="running",
                    attributes={
                        "tool_call_id": call_id,
                        "intent_action_id": (
                            intent.get("action_id") if isinstance(intent, dict) else None
                        ),
                        "skill_id": (intent.get("skill_id") if isinstance(intent, dict) else None),
                    },
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            return 1

        if event.type == "tool.result":
            call_id = str(event.data.get("tool_call_id") or _id("tool"))
            failed = bool(event.data.get("is_error", False))
            world.upsert(
                "action",
                ActionRecord(
                    id=f"action:{call_id}",
                    type=str(event.data.get("tool") or "tool"),
                    status="failed" if failed else "succeeded",
                    attributes={
                        "tool_call_id": call_id,
                        "duration_ms": event.data.get("duration_ms"),
                        "failure_class": event.data.get("failure_class"),
                        "skill_id": (
                            progress.action_intent.get("skill_id")
                            if isinstance(progress.action_intent, dict)
                            else None
                        ),
                    },
                ),
                actor=actor,
                source_event_id=event.event_id,
            )
            world.upsert(
                "observation",
                Observation(
                    id=f"obs:tool:{call_id}",
                    type="tool.execution",
                    content={
                        "tool": event.data.get("tool"),
                        "tool_call_id": call_id,
                        "status": "failed" if failed else "succeeded",
                        "duration_ms": event.data.get("duration_ms"),
                        "failure_class": event.data.get("failure_class"),
                    },
                    confidence=1.0,
                    source="harness.tool",
                    provenance=Provenance(
                        source="harness.tool",
                        epistemic_status="evidence",
                    ),
                ),
                actor="harness",
                source_event_id=event.event_id,
            )
            return 2

        return 0
