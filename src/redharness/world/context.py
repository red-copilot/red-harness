from __future__ import annotations

import json
from typing import Any

from .models import WorldSnapshot
from .retrieval import WorldRetriever


class WorldContextBuilder:
    """Build a compact, deterministic Agent-facing projection of world state."""

    def __init__(
        self,
        *,
        max_observations: int = 20,
        max_failures: int = 10,
        max_relations: int = 20,
        max_chars: int = 16000,
        retriever: WorldRetriever | None = None,
    ) -> None:
        self.max_observations = max_observations
        self.max_failures = max_failures
        self.max_relations = max_relations
        self.max_chars = max_chars
        self.retriever = retriever or WorldRetriever(
            limits={
                "observations": max_observations,
                "failures": max_failures,
                "relations": max_relations,
            }
        )

    @staticmethod
    def _valid_values(collection: dict) -> list[Any]:
        return [item for item in collection.values() if item.is_valid_at()]

    @staticmethod
    def _state_meta(item: Any) -> dict:
        return item.model_dump(
            mode="json",
            include={
                "provenance",
                "observed_at",
                "valid_from",
                "expires_at",
                "supersedes",
            },
            exclude_none=True,
        )

    @staticmethod
    def _line(prefix: str, object_id: str, type_name: str, payload: dict) -> str:
        compact = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        return f"- {prefix} {object_id} [{type_name}] {compact}"

    def render(self, snapshot: WorldSnapshot, *, query: str | None = None) -> str:
        selected = self.retriever.retrieve(snapshot, query=query)
        lines = [
            "# Red Harness World Context",
            f"revision: {snapshot.revision}",
            "",
            "## Goals",
        ]

        goals = sorted(
            selected["goals"],
            key=lambda item: (-item.priority, item.id),
        )
        if goals:
            for goal in goals:
                lines.append(
                    self._line(
                        goal.status,
                        goal.id,
                        "goal",
                        {
                            "description": goal.description,
                            "priority": goal.priority,
                            "parent_id": goal.parent_id,
                            **goal.attributes,
                            **self._state_meta(goal),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Entities"])
        entities = sorted(selected["entities"], key=lambda item: item.id)
        if entities:
            for entity in entities:
                lines.append(
                    self._line(
                        "entity",
                        entity.id,
                        entity.type,
                        {**entity.attributes, **self._state_meta(entity)},
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Capabilities"])
        capabilities = sorted(selected["capabilities"], key=lambda item: item.id)
        if capabilities:
            for capability in capabilities:
                lines.append(
                    self._line(
                        "capability",
                        capability.id,
                        capability.type,
                        {
                            "subject": capability.subject,
                            "scope": capability.scope,
                            **capability.attributes,
                            **self._state_meta(capability),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Open hypotheses"])
        hypotheses = sorted(selected["hypotheses"], key=lambda item: (-item.confidence, item.id))
        hypotheses = [item for item in hypotheses if item.status in {"open", "supported"}]
        if hypotheses:
            for hypothesis in hypotheses:
                lines.append(
                    self._line(
                        hypothesis.status,
                        hypothesis.id,
                        "hypothesis",
                        {
                            "statement": hypothesis.statement,
                            "confidence": hypothesis.confidence,
                            **hypothesis.attributes,
                            **self._state_meta(hypothesis),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Artifacts"])
        artifacts = sorted(selected["artifacts"], key=lambda item: item.id)
        if artifacts:
            for artifact in artifacts:
                lines.append(
                    self._line(
                        "artifact",
                        artifact.id,
                        artifact.type,
                        {"uri": artifact.uri, **artifact.attributes, **self._state_meta(artifact)},
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Observations"])
        observations = sorted(selected["observations"], key=lambda item: item.id)
        observations = observations[-self.max_observations :]
        if observations:
            for observation in observations:
                lines.append(
                    self._line(
                        "observation",
                        observation.id,
                        observation.type,
                        {
                            "confidence": observation.confidence,
                            "source": observation.source,
                            **observation.content,
                            **self._state_meta(observation),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Actions"])
        actions = sorted(selected["actions"], key=lambda item: item.id)
        if actions:
            for action in actions:
                lines.append(
                    self._line(
                        action.status,
                        action.id,
                        action.type,
                        {"target": action.target, **action.attributes, **self._state_meta(action)},
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Constraints"])
        constraints = sorted(selected["constraints"], key=lambda item: item.id)
        if constraints:
            for constraint in constraints:
                lines.append(
                    self._line(
                        constraint.status,
                        constraint.id,
                        constraint.type,
                        {
                            "description": constraint.description,
                            "scope": constraint.scope,
                            **constraint.attributes,
                            **self._state_meta(constraint),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Failures"])
        failures = sorted(selected["failures"], key=lambda item: item.id)
        failures = failures[-self.max_failures :]
        if failures:
            for failure in failures:
                lines.append(
                    self._line(
                        "failure",
                        failure.id,
                        failure.type,
                        {
                            "message": failure.message,
                            "action_id": failure.action_id,
                            "recoverable": failure.recoverable,
                            **failure.attributes,
                            **self._state_meta(failure),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Relations"])
        relations = sorted(selected["relations"], key=lambda item: item.id)
        relations = relations[-self.max_relations :]
        if relations:
            for relation in relations:
                lines.append(
                    self._line(
                        "relation",
                        relation.id,
                        relation.type,
                        {
                            "source": relation.source,
                            "target": relation.target,
                            **relation.attributes,
                            **self._state_meta(relation),
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(
            [
                "",
                (
                    "Use this as durable task state, not as ground truth. Observations and "
                    "hypotheses may be incomplete, stale, or conflicting."
                ),
            ]
        )
        rendered = "\n".join(lines) + "\n"
        if len(rendered) <= self.max_chars:
            return rendered

        marker = "[context truncated to character budget]"
        kept: list[str] = []
        used = 0
        for line in lines:
            addition = len(line) + 1
            if used + addition + len(marker) + 1 > self.max_chars:
                break
            kept.append(line)
            used += addition
        kept.append(marker)
        return "\n".join(kept) + "\n"
