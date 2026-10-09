from __future__ import annotations

from .progress import ProgressLedger
from .verification import (
    ActionVerdict,
    AuthorizedActionVerdict,
    EvidenceRef,
    VerifierRegistry,
    now_utc,
)

# Backward compatible import name. The serialized verdict now has typed IDs,
# producer, capture time, execution status, world revision and evidence refs.
ActionVerification = ActionVerdict


class ActionVerifier:
    """Conservative verifier: Agent text cannot establish a verified effect.

    Only a typed verdict handed in by Harness-controlled integration code is
    considered. Agent event dictionaries never enter this channel.
    """

    def __init__(self, registry: VerifierRegistry | None = None) -> None:
        self.registry = registry or VerifierRegistry()

    def verify(
        self,
        progress: ProgressLedger,
        *,
        run_id: str,
        planned_world_revision: int | None = None,
        current_world_revision: int | None = None,
        evidence_world_revision: int | None = None,
        trusted_verdict: AuthorizedActionVerdict | None = None,
    ) -> ActionVerdict | None:
        expected = progress.expected_observation
        actual = progress.actual_observation
        last_action = progress.last_action or {}
        action_id = last_action.get("tool_call_id")
        if not isinstance(action_id, str) or not action_id or len(action_id) > 256:
            return None

        execution_status = last_action.get("status")
        if execution_status not in {"succeeded", "failed"}:
            execution_status = "unknown"

        reasons: list[str] = []
        revision = current_world_revision
        if revision is None:
            revision = planned_world_revision if planned_world_revision is not None else 0
        trusted_revision = evidence_world_revision
        if trusted_revision is None:
            trusted_revision = current_world_revision

        world_revision_changed = (
            planned_world_revision is not None
            and current_world_revision is not None
            and planned_world_revision != current_world_revision
        )
        if world_revision_changed:
            reasons.append("world_revision_changed")

        if execution_status == "failed":
            return self._verdict(
                run_id=run_id,
                action_id=action_id,
                revision=revision,
                execution_status="failed",
                status="pending",
                expected=expected,
                actual=actual,
                reasons=("action_failed",),
            )

        verdict = (
            trusted_verdict.verdict if self.registry.is_authorized_action(trusted_verdict) else None
        )
        if verdict is not None:
            refs_match = bool(verdict.evidence) and all(
                ref.run_id == run_id
                and ref.action_id == action_id
                and ref.producer == verdict.producer
                and trusted_revision is not None
                and ref.world_revision == trusted_revision
                for ref in verdict.evidence
            )
            verdict_matches = (
                verdict.run_id == run_id
                and verdict.action_id == action_id
                and verdict.producer in {"tool_adapter", "task_verifier", "benchmark_evaluator"}
                and verdict.execution_status == "succeeded"
                and trusted_revision is not None
                and verdict.world_revision == trusted_revision
                and refs_match
            )
            if verdict_matches:
                reasons = list(verdict.replan_reasons)
                if verdict.status == "contradicted":
                    reasons.append("trusted_evidence_contradicted")
                return self._verdict(
                    run_id=run_id,
                    action_id=action_id,
                    revision=trusted_revision,
                    execution_status="succeeded",
                    status=verdict.status,
                    expected=expected,
                    actual=(verdict.actual_observation or actual),
                    evidence=verdict.evidence,
                    reasons=reasons,
                )
            has_stale_revision = trusted_revision is not None and (
                verdict.world_revision != trusted_revision
                or any(ref.world_revision != trusted_revision for ref in verdict.evidence)
            )
            if verdict.run_id == run_id and verdict.action_id == action_id and has_stale_revision:
                reasons.append("stale_evidence_revision")

        if expected is None:
            return None

        if actual is None:
            reasons.append("expected_observation_missing")
            return self._verdict(
                run_id=run_id,
                action_id=action_id,
                revision=revision,
                execution_status=execution_status,
                status="pending",
                expected=expected,
                actual=None,
                reasons=reasons,
            )

        reasons.append("unverified_observation")
        return self._verdict(
            run_id=run_id,
            action_id=action_id,
            revision=revision,
            execution_status=execution_status,
            status="pending",
            expected=expected,
            actual=actual,
            reasons=reasons,
        )

    @staticmethod
    def _verdict(
        *,
        run_id: str,
        action_id: str,
        revision: int,
        execution_status: str,
        status: str,
        expected: str | None,
        actual: str | None,
        evidence: tuple[EvidenceRef, ...] = (),
        reasons: tuple[str, ...] | list[str] = (),
    ) -> ActionVerdict:
        unique_reasons = tuple(dict.fromkeys(reasons))
        return ActionVerdict(
            run_id=run_id,
            action_id=action_id,
            producer="harness_verifier",
            captured_at=now_utc(),
            world_revision=revision,
            execution_status=execution_status,  # type: ignore[arg-type]
            status=status,  # type: ignore[arg-type]
            expected_observation=expected,
            actual_observation=actual,
            evidence=evidence,
            replan_required=status != "verified" or bool(unique_reasons),
            replan_reasons=unique_reasons,
        )
