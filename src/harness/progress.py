from __future__ import annotations

import json
import os
import tempfile
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field, field_validator

from .session import AgentEvent
from .verification import AuthorizedObjectiveVerdict, VerifierRegistry


class ProgressHypothesis(BaseModel):
    id: str
    statement: str | None = None
    status: str = "open"
    confidence: float | None = None
    evidence_for: list[str] = Field(default_factory=list)
    evidence_against: list[str] = Field(default_factory=list)


class ProgressLedger(BaseModel):
    schema_version: str = "harness/progress/v3"
    started_at: datetime = Field(default_factory=lambda: datetime.now(UTC))
    active_goal: str | None = None
    completed_subgoals: list[str] = Field(default_factory=list)
    claimed_completed_subgoals: list[str] = Field(default_factory=list)
    completion_claims: list[str] = Field(default_factory=list)
    blocked_subgoals: list[str] = Field(default_factory=list)
    claims: list[str] = Field(default_factory=list)
    # Kept for readers of v1 progress files. Agent-authored facts are no
    # longer added here; only independently verified facts belong here.
    confirmed_facts: list[str] = Field(default_factory=list)
    current_subgoal: str | None = None
    hypotheses: dict[str, ProgressHypothesis] = Field(default_factory=dict)
    action_intent: dict[str, Any] | None = None
    expected_observation: str | None = None
    actual_observation: str | None = None
    replan_reasons: list[str] = Field(default_factory=list)
    last_verification: dict[str, Any] | None = None
    verified_actions: int = 0
    execution_succeeded: int = 0
    evidence_confirmed: int = 0
    capability_acquired: int = 0
    goal_advanced: int = 0
    contradicted_actions: int = 0
    pending_actions: int = 0
    skipped_verifications: int = 0
    last_action: dict[str, Any] | None = None
    active_actions: dict[str, dict[str, Any]] = Field(default_factory=dict)
    failure_count: int = 0
    no_progress_count: int = 0
    replan_count: int = 0
    failed_skill_counts: dict[str, int] = Field(default_factory=dict)
    reconciled_actions: dict[str, str] = Field(default_factory=dict)
    # Correlates legacy events that predate tool_call_id, so replay uses the
    # same generated action identity after a checkpoint reload.
    tool_event_correlations: dict[str, str] = Field(default_factory=dict)
    budget_limits: dict[str, float] = Field(default_factory=dict)
    budget_used: dict[str, float] = Field(default_factory=dict)
    tool_calls: int = 0
    model_calls: int = 0
    accepted_submissions: int = 0
    rejected_submissions: int = 0
    objective_completed: bool = False
    objective_verdict: dict[str, Any] | None = None
    last_event_type: str | None = None
    last_event_id: str | None = None
    event_count: int = 0

    @field_validator("started_at")
    @classmethod
    def validate_started_at(cls, value: datetime) -> datetime:
        if value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("started_at must be timezone-aware")
        return value

    def record_event(self, event: AgentEvent) -> None:
        self.event_count += 1
        self.last_event_type = event.type
        self.last_event_id = event.event_id
        if event.type == "tool.call":
            self.tool_calls += 1
            self.actual_observation = None
            action = {
                "tool": event.data.get("tool"),
                "tool_call_id": event.data.get("tool_call_id"),
                "status": "running",
                "skill_id": (self.action_intent or {}).get("skill_id"),
            }
            self.last_action = action
            call_id = action["tool_call_id"]
            if isinstance(call_id, str) and call_id:
                self.active_actions[call_id] = dict(action)
            expected = event.data.get("expected_observation")
            if isinstance(expected, str) and expected:
                self.expected_observation = expected
        elif event.type == "tool.result":
            failed = bool(event.data.get("is_error", False))
            call_id = event.data.get("tool_call_id")
            active_action = self.active_actions.pop(call_id, {}) if isinstance(call_id, str) else {}
            action = {
                "tool": event.data.get("tool"),
                "tool_call_id": call_id,
                "status": "failed" if failed else "succeeded",
                "skill_id": active_action.get("skill_id")
                or (self.action_intent or {}).get("skill_id"),
            }
            self.last_action = action
            actual = event.data.get("observation")
            if isinstance(actual, str) and actual:
                self.actual_observation = actual
            if failed:
                self.failure_count += 1
                skill_id = action.get("skill_id")
                if isinstance(skill_id, str) and skill_id:
                    self.failed_skill_counts[skill_id] = (
                        self.failed_skill_counts.get(skill_id, 0) + 1
                    )
            else:
                self.execution_succeeded += 1
            # A process exit is execution evidence only. It does not prove a
            # target finding, acquired capability, or goal progress.
            self.no_progress_count += 1
        elif event.type == "progress.updated":
            self._record_progress_update(event.data)
        elif event.type == "goal.completed":
            claim = event.data.get("goal_id") or self.active_goal or "objective"
            if isinstance(claim, str) and claim not in self.completion_claims:
                self.completion_claims.append(claim)
        elif event.type == "model.request":
            count = event.data.get("count", 1)
            if isinstance(count, int) and count > 0:
                self.model_calls += count
        elif event.type == "model.usage":
            usage = event.data
            for field, source in (
                ("tokens", "total_tokens"),
                ("cost_usd", "cost_usd"),
            ):
                value = usage.get(source)
                if isinstance(value, (int, float)) and value >= 0:
                    self.budget_used[field] = self.budget_used.get(field, 0.0) + float(value)

    def record_intent(self, intent: Any) -> None:
        data = intent.model_dump(mode="json") if hasattr(intent, "model_dump") else dict(intent)
        self.action_intent = data
        skill_id = data.get("skill_id")
        if isinstance(skill_id, str) and skill_id:
            self.action_intent["skill_id"] = skill_id
        subgoal = data.get("subgoal_id")
        if isinstance(subgoal, str) and subgoal:
            self.current_subgoal = subgoal
        expected = data.get("expected_observations") or []
        self.expected_observation = (
            str(expected[0]) if isinstance(expected, list) and expected else None
        )
        self.actual_observation = None

    def _record_progress_update(self, data: dict[str, Any]) -> None:
        current = data.get("current_subgoal", data.get("subgoal"))
        if isinstance(current, str) and current:
            self.current_subgoal = current

        completed = data.get("completed_subgoal")
        if isinstance(completed, str) and completed not in self.claimed_completed_subgoals:
            self.claimed_completed_subgoals.append(completed)

        if data.get("objective_completed") is True:
            claim = str(data.get("goal_id") or self.active_goal or "objective")
            if claim not in self.completion_claims:
                self.completion_claims.append(claim)

        blocked = data.get("blocked_subgoal")
        if isinstance(blocked, str) and blocked not in self.blocked_subgoals:
            self.blocked_subgoals.append(blocked)

        fact = data.get("confirmed_fact")
        if isinstance(fact, str) and fact not in self.claims:
            self.claims.append(fact)

        expected = data.get("expected_observation")
        if isinstance(expected, str) and expected:
            self.expected_observation = expected

        actual = data.get("actual_observation")
        if isinstance(actual, str) and actual:
            self.actual_observation = actual

        hypothesis = data.get("hypothesis")
        if isinstance(hypothesis, dict) and isinstance(hypothesis.get("id"), str):
            item = ProgressHypothesis.model_validate(hypothesis)
            self.hypotheses[item.id] = item

        replan_reason = data.get("replan_reason")
        if (
            isinstance(replan_reason, str)
            and replan_reason
            and replan_reason not in self.replan_reasons
        ):
            self.replan_reasons.append(replan_reason)

        made_progress = data.get("made_progress")
        if made_progress is False:
            self.no_progress_count += 1

    def record_verification(self, verification: Any) -> None:
        data = (
            verification.model_dump(mode="json")
            if hasattr(verification, "model_dump")
            else dict(verification)
        )
        self.last_verification = data
        status = data.get("status")
        if status == "verified":
            self.verified_actions += 1
            self.evidence_confirmed += 1
            self.no_progress_count = 0
        elif status == "contradicted":
            self.contradicted_actions += 1
            if (self.last_action or {}).get("status") != "failed":
                self.failure_count += 1
                self.no_progress_count += 1
        elif status == "pending":
            self.pending_actions += 1

        for reason in data.get("replan_reasons", []):
            if reason not in self.replan_reasons:
                self.replan_reasons.append(reason)

    def record_submission(self, *, accepted: bool, completed: bool) -> None:
        del completed  # Final evaluation, not submission feedback, owns completion.
        if accepted:
            self.accepted_submissions += 1
            if "benchmark_negative_feedback" in self.replan_reasons:
                self.replan_reasons.remove("benchmark_negative_feedback")
        else:
            self.rejected_submissions += 1
            self.failure_count += 1
            self.no_progress_count += 1
            if "benchmark_negative_feedback" not in self.replan_reasons:
                self.replan_reasons.append("benchmark_negative_feedback")
        # Submission feedback may ask the runtime to stop early, but only the
        # final evaluator can establish objective completion.

    def record_trusted_progress(self, kind: str) -> None:
        """Record progress only from a Harness-owned verifier or benchmark."""
        if kind == "evidence_confirmed":
            self.evidence_confirmed += 1
        elif kind == "capability_acquired":
            self.capability_acquired += 1
        elif kind == "goal_advanced":
            self.goal_advanced += 1
        elif kind == "objective_completed":
            raise ValueError("objective completion requires an authorized ObjectiveVerdict")
        else:
            raise ValueError(f"unknown trusted progress kind: {kind}")
        self.no_progress_count = 0

    def record_objective_verdict(
        self,
        authorized: AuthorizedObjectiveVerdict,
        registry: VerifierRegistry,
    ) -> None:
        if not registry.is_authorized_objective(authorized):
            raise ValueError("objective verdict was not authorized by the run verifier registry")
        verdict = authorized.verdict
        self.objective_verdict = verdict.model_dump(mode="json")
        self.objective_completed = verdict.status == "verified"
        if self.objective_completed:
            self.no_progress_count = 0

    def consume_replan_reasons(self, reasons: list[str]) -> None:
        consumed = set(reasons)
        self.replan_reasons = [reason for reason in self.replan_reasons if reason not in consumed]

    def request_replan(self, reason: str) -> None:
        if reason and reason not in self.replan_reasons:
            self.replan_reasons.append(reason)

    @property
    def remaining_budget_fraction(self) -> float:
        remaining = [1.0]
        for key, limit in self.budget_limits.items():
            if limit <= 0:
                continue
            if key == "wall_time":
                elapsed = max(0.0, (datetime.now(UTC) - self.started_at).total_seconds())
                remaining.append(max(0.0, min(1.0, (limit - elapsed) / limit)))
                continue
            if key == "max_model_calls":
                used = float(self.model_calls)
            elif key == "max_tool_calls":
                used = float(self.tool_calls)
            elif key == "max_tokens":
                used = self.budget_used.get("tokens", 0.0)
            elif key == "max_cost_usd":
                used = self.budget_used.get("cost_usd", 0.0)
            else:
                continue
            remaining.append(max(0.0, min(1.0, (limit - used) / limit)))
        return min(remaining)

    def write(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        payload = json.dumps(self.model_dump(mode="json"), ensure_ascii=False, indent=2) + "\n"
        descriptor, temporary_name = tempfile.mkstemp(
            dir=path.parent, prefix=f".{path.name}.", suffix=".tmp"
        )
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary_name, path)
            if hasattr(os, "O_DIRECTORY"):
                directory_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
        finally:
            if os.path.exists(temporary_name):
                os.unlink(temporary_name)
