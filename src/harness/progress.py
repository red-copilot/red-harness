from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field

from .session import AgentEvent


class ProgressHypothesis(BaseModel):
    id: str
    statement: str | None = None
    status: str = "open"
    confidence: float | None = None
    evidence_for: list[str] = Field(default_factory=list)
    evidence_against: list[str] = Field(default_factory=list)


class ProgressLedger(BaseModel):
    schema_version: str = "harness/progress/v1"
    active_goal: str | None = None
    completed_subgoals: list[str] = Field(default_factory=list)
    blocked_subgoals: list[str] = Field(default_factory=list)
    confirmed_facts: list[str] = Field(default_factory=list)
    current_subgoal: str | None = None
    hypotheses: dict[str, ProgressHypothesis] = Field(default_factory=dict)
    action_intent: dict[str, Any] | None = None
    expected_observation: str | None = None
    actual_observation: str | None = None
    replan_reasons: list[str] = Field(default_factory=list)
    last_verification: dict[str, Any] | None = None
    verified_actions: int = 0
    contradicted_actions: int = 0
    pending_actions: int = 0
    skipped_verifications: int = 0
    last_action: dict[str, Any] | None = None
    failure_count: int = 0
    no_progress_count: int = 0
    accepted_submissions: int = 0
    rejected_submissions: int = 0
    objective_completed: bool = False
    last_event_type: str | None = None

    def record_event(self, event: AgentEvent) -> None:
        self.last_event_type = event.type
        if event.type == "tool.call":
            self.actual_observation = None
            self.last_action = {
                "tool": event.data.get("tool"),
                "tool_call_id": event.data.get("tool_call_id"),
                "status": "running",
            }
            expected = event.data.get("expected_observation")
            if isinstance(expected, str) and expected:
                self.expected_observation = expected
        elif event.type == "tool.result":
            failed = bool(event.data.get("is_error", False))
            self.last_action = {
                "tool": event.data.get("tool"),
                "tool_call_id": event.data.get("tool_call_id"),
                "status": "failed" if failed else "succeeded",
            }
            actual = event.data.get("observation")
            if isinstance(actual, str) and actual:
                self.actual_observation = actual
            if failed:
                self.failure_count += 1
                self.no_progress_count += 1
            else:
                self.no_progress_count = 0
        elif event.type == "progress.updated":
            self._record_progress_update(event.data)
        elif event.type in {"capability.acquired", "goal.completed"}:
            self.no_progress_count = 0


    def record_intent(self, intent: Any) -> None:
        data = (
            intent.model_dump(mode="json")
            if hasattr(intent, "model_dump")
            else dict(intent)
        )
        self.action_intent = data
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
        if isinstance(completed, str) and completed not in self.completed_subgoals:
            self.completed_subgoals.append(completed)

        blocked = data.get("blocked_subgoal")
        if isinstance(blocked, str) and blocked not in self.blocked_subgoals:
            self.blocked_subgoals.append(blocked)

        fact = data.get("confirmed_fact")
        if isinstance(fact, str) and fact not in self.confirmed_facts:
            self.confirmed_facts.append(fact)

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
        if made_progress is True:
            self.no_progress_count = 0
        elif made_progress is False:
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
        if accepted:
            self.accepted_submissions += 1
            self.no_progress_count = 0
            if "benchmark_negative_feedback" in self.replan_reasons:
                self.replan_reasons.remove("benchmark_negative_feedback")
        else:
            self.rejected_submissions += 1
            self.failure_count += 1
            self.no_progress_count += 1
            if "benchmark_negative_feedback" not in self.replan_reasons:
                self.replan_reasons.append("benchmark_negative_feedback")
        if completed:
            self.objective_completed = True

    def write(self, path: Path) -> None:
        path.write_text(
            json.dumps(self.model_dump(mode="json"), ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
