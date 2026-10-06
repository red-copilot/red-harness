from __future__ import annotations

import json

from .models import WorldSnapshot


class WorldContextBuilder:
    """Build a compact, deterministic Agent-facing projection of world state."""

    def __init__(
        self,
        *,
        max_observations: int = 20,
        max_failures: int = 10,
        max_relations: int = 20,
    ) -> None:
        self.max_observations = max_observations
        self.max_failures = max_failures
        self.max_relations = max_relations

    @staticmethod
    def _line(prefix: str, object_id: str, type_name: str, payload: dict) -> str:
        compact = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        return f"- {prefix} {object_id} [{type_name}] {compact}"

    def render(self, snapshot: WorldSnapshot) -> str:
        lines = [
            "# Red Harness World Context",
            f"revision: {snapshot.revision}",
            "",
            "## Goals",
        ]

        goals = sorted(
            snapshot.goals.values(),
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
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Entities"])
        entities = sorted(snapshot.entities.values(), key=lambda item: item.id)
        if entities:
            for entity in entities:
                lines.append(
                    self._line(
                        "entity",
                        entity.id,
                        entity.type,
                        entity.attributes,
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Capabilities"])
        capabilities = sorted(snapshot.capabilities.values(), key=lambda item: item.id)
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
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Open hypotheses"])
        hypotheses = sorted(snapshot.hypotheses.values(), key=lambda item: (-item.confidence, item.id))
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
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Artifacts"])
        artifacts = sorted(snapshot.artifacts.values(), key=lambda item: item.id)
        if artifacts:
            for artifact in artifacts:
                lines.append(
                    self._line(
                        "artifact",
                        artifact.id,
                        artifact.type,
                        {"uri": artifact.uri, **artifact.attributes},
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Observations"])
        observations = sorted(snapshot.observations.values(), key=lambda item: item.id)
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
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Actions"])
        actions = sorted(snapshot.actions.values(), key=lambda item: item.id)
        if actions:
            for action in actions:
                lines.append(
                    self._line(
                        action.status,
                        action.id,
                        action.type,
                        {"target": action.target, **action.attributes},
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Constraints"])
        constraints = sorted(snapshot.constraints.values(), key=lambda item: item.id)
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
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Failures"])
        failures = sorted(snapshot.failures.values(), key=lambda item: item.id)
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
                        },
                    )
                )
        else:
            lines.append("- none")

        lines.extend(["", "## Relations"])
        relations = sorted(snapshot.relations.values(), key=lambda item: item.id)
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
        return "\n".join(lines) + "\n"
