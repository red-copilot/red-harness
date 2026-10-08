from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, Field

from .progress import ProgressLedger


class ActionVerification(BaseModel):
    status: Literal["verified", "contradicted", "pending"]
    expected_observation: str | None = None
    actual_observation: str | None = None
    evidence: list[str] = Field(default_factory=list)
    replan_required: bool = False
    replan_reasons: list[str] = Field(default_factory=list)


class ActionVerifier:
    """Conservative verifier: agent text cannot establish verified success.

    trusted_evidence must come from harness-controlled code, never agent payloads.
    """

    def verify(
        self,
        progress: ProgressLedger,
        *,
        planned_world_revision: int | None = None,
        current_world_revision: int | None = None,
        trusted_evidence: dict | None = None,
    ) -> ActionVerification | None:
        expected = progress.expected_observation
        actual = progress.actual_observation
        last_action = progress.last_action or {}

        reasons: list[str] = []
        evidence: list[str] = []

        if last_action.get("status") == "failed":
            reasons.append("action_failed")
            return ActionVerification(
                status="contradicted",
                expected_observation=expected,
                actual_observation=actual,
                evidence=["last action failed"],
                replan_required=True,
                replan_reasons=reasons,
            )

        world_revision_changed = (
            planned_world_revision is not None
            and current_world_revision is not None
            and planned_world_revision != current_world_revision
        )
        if world_revision_changed:
            evidence.append(f"world_revision={planned_world_revision}->{current_world_revision}")

        # Textual observations, including world.observe from an agent, are claims.
        # Only a Harness-owned producer can supply this separate argument.
        if trusted_evidence is not None:
            source = trusted_evidence.get("source")
            verdict = trusted_evidence.get("verdict")
            action_id = trusted_evidence.get("action_id")
            actual_id = last_action.get("tool_call_id")
            if (
                source in {"tool_adapter", "task_verifier"}
                and verdict in {"verified", "contradicted"}
                and isinstance(action_id, str)
                and bool(action_id)
                and action_id == actual_id
            ):
                evidence_ref = trusted_evidence.get("evidence_id")
                trusted_observation = trusted_evidence.get("observation")
                return ActionVerification(
                    status=verdict,
                    expected_observation=expected,
                    actual_observation=(
                        trusted_observation if isinstance(trusted_observation, str) else actual
                    ),
                    evidence=[
                        f"trusted:{source}:{action_id}"
                        + (f":{evidence_ref}" if isinstance(evidence_ref, str) else "")
                    ],
                    replan_required=verdict == "contradicted",
                    replan_reasons=(
                        ["trusted_evidence_contradicted"] if verdict == "contradicted" else []
                    ),
                )

        if expected is None:
            return None

        if actual is None:
            reasons.append("expected_observation_missing")
            if world_revision_changed:
                reasons.append("world_revision_changed")
            return ActionVerification(
                status="pending",
                expected_observation=expected,
                evidence=["expected observation has no actual observation yet"],
                replan_required=True,
                replan_reasons=reasons,
            )

        reasons.append("unverified_observation")
        if world_revision_changed:
            reasons.append("world_revision_changed")
        return ActionVerification(
            status="pending",
            expected_observation=expected,
            actual_observation=actual,
            evidence=["agent observations are not independently verified"],
            replan_required=True,
            replan_reasons=reasons,
        )
