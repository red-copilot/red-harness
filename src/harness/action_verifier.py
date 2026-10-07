# ruff: noqa: I001
from __future__ import annotations

import re
from typing import Literal

from pydantic import BaseModel, Field

from .progress import ProgressLedger


_TOKEN_RE = re.compile(r"[A-Za-z0-9]{3,}")


class ActionVerification(BaseModel):
    status: Literal["verified", "contradicted", "pending"]
    expected_observation: str | None = None
    actual_observation: str | None = None
    evidence: list[str] = Field(default_factory=list)
    replan_required: bool = False
    replan_reasons: list[str] = Field(default_factory=list)


class ActionVerifier:
    """Conservative deterministic verifier for solver action outcomes."""

    @staticmethod
    def _tokens(value: str | None) -> set[str]:
        if not value:
            return set()
        return {token.lower() for token in _TOKEN_RE.findall(value)}

    def verify(
        self,
        progress: ProgressLedger,
        *,
        planned_world_revision: int | None = None,
        current_world_revision: int | None = None,
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
            evidence.append(
                f"world_revision={planned_world_revision}->{current_world_revision}"
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

        expected_tokens = self._tokens(expected)
        actual_tokens = self._tokens(actual)
        overlap = expected_tokens & actual_tokens
        coverage = len(overlap) / max(1, len(expected_tokens))

        explicit_negative = any(
            token in actual.lower()
            for token in ("not found", "missing", "closed", "denied", "failed", "unreachable")
        )
        if explicit_negative:
            reasons.append("expected_observation_contradicted")
            evidence.append("actual observation contains explicit negative evidence")
            return ActionVerification(
                status="contradicted",
                expected_observation=expected,
                actual_observation=actual,
                evidence=evidence,
                replan_required=True,
                replan_reasons=reasons,
            )

        if expected.lower() in actual.lower() or coverage >= 0.6:
            evidence.append(f"token_coverage={coverage:.2f}")
            return ActionVerification(
                status="verified",
                expected_observation=expected,
                actual_observation=actual,
                evidence=evidence,
                replan_required=bool(reasons),
                replan_reasons=reasons,
            )

        reasons.append("expected_observation_not_evidenced")
        if world_revision_changed:
            reasons.append("world_revision_changed")
        evidence.append(f"token_coverage={coverage:.2f}")
        return ActionVerification(
            status="pending",
            expected_observation=expected,
            actual_observation=actual,
            evidence=evidence,
            replan_required=True,
            replan_reasons=reasons,
        )
