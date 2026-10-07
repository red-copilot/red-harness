# ruff: noqa: I001
from __future__ import annotations

import re
from typing import Literal

from pydantic import BaseModel, Field

from .progress import ProgressLedger


_TOKEN_RE = re.compile(r"[A-Za-z0-9]{3,}")
_NEGATIVE_PHRASES = (
    "not found",
    "missing",
    "closed",
    "denied",
    "failed",
    "unreachable",
    "not reachable",
    "not accessible",
    "refused",
    "forbidden",
)


class ActionVerification(BaseModel):
    status: Literal["verified", "contradicted", "pending"]
    expected_observation: str | None = None
    expected_observations: list[str] = Field(default_factory=list)
    actual_observation: str | None = None
    evidence: list[str] = Field(default_factory=list)
    replan_required: bool = False
    replan_reasons: list[str] = Field(default_factory=list)


class ActionVerifier:
    """Conservative verifier over action-bound semantic observations."""

    @staticmethod
    def _tokens(value: str | None) -> set[str]:
        if not value:
            return set()
        return {token.lower() for token in _TOKEN_RE.findall(value)}

    @classmethod
    def _coverage(cls, expected: str, actual: str) -> float:
        expected_tokens = cls._tokens(expected)
        actual_tokens = cls._tokens(actual)
        return len(expected_tokens & actual_tokens) / max(1, len(expected_tokens))

    def verify(
        self,
        progress: ProgressLedger,
        *,
        planned_world_revision: int | None = None,
        current_world_revision: int | None = None,
    ) -> ActionVerification | None:
        expected_values = list(progress.expected_observations)
        if not expected_values and progress.expected_observation:
            expected_values = [progress.expected_observation]
        if not expected_values:
            return None

        actual = progress.actual_observation
        intent = progress.action_intent or {}
        intent_action_id = intent.get("action_id")
        if (
            actual is not None
            and isinstance(intent_action_id, str)
            and progress.actual_observation_action_id != intent_action_id
        ):
            actual = None

        evidence: list[str] = []
        world_revision_changed = (
            planned_world_revision is not None
            and current_world_revision is not None
            and planned_world_revision != current_world_revision
        )
        if world_revision_changed:
            evidence.append(
                f"world_revision={planned_world_revision}->{current_world_revision}"
            )

        if actual is None:
            return ActionVerification(
                status="pending",
                expected_observation=expected_values[0],
                expected_observations=expected_values,
                evidence=[*evidence, "waiting for action-bound semantic observation"],
                replan_required=False,
            )

        scored = sorted(
            (
                (self._coverage(expected, actual), expected)
                for expected in expected_values
            ),
            reverse=True,
        )
        best_coverage, best_expected = scored[0]
        actual_lower = actual.lower()
        expected_lower = best_expected.lower()

        actual_negative = any(phrase in actual_lower for phrase in _NEGATIVE_PHRASES)
        expected_negative = any(phrase in expected_lower for phrase in _NEGATIVE_PHRASES)
        if actual_negative and not expected_negative:
            reasons = ["expected_observation_contradicted"]
            if world_revision_changed:
                reasons.append("world_revision_changed")
            return ActionVerification(
                status="contradicted",
                expected_observation=best_expected,
                expected_observations=expected_values,
                actual_observation=actual,
                evidence=[
                    *evidence,
                    "actual observation contains explicit negative evidence",
                ],
                replan_required=True,
                replan_reasons=reasons,
            )

        if expected_lower in actual_lower or best_coverage >= 0.6:
            return ActionVerification(
                status="verified",
                expected_observation=best_expected,
                expected_observations=expected_values,
                actual_observation=actual,
                evidence=[*evidence, f"token_coverage={best_coverage:.2f}"],
            )

        reasons = ["expected_observation_not_evidenced"]
        if world_revision_changed:
            reasons.append("world_revision_changed")
        return ActionVerification(
            status="pending",
            expected_observation=best_expected,
            expected_observations=expected_values,
            actual_observation=actual,
            evidence=[*evidence, f"token_coverage={best_coverage:.2f}"],
            replan_required=True,
            replan_reasons=reasons,
        )
