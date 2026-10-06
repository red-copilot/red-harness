from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field

from .session import AgentEvent


class ProgressLedger(BaseModel):
    schema_version: str = "redharness.progress/v1"
    active_goal: str | None = None
    completed_subgoals: list[str] = Field(default_factory=list)
    blocked_subgoals: list[str] = Field(default_factory=list)
    confirmed_facts: list[str] = Field(default_factory=list)
    current_subgoal: str | None = None
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
            self.last_action = {
                "tool": event.data.get("tool"),
                "tool_call_id": event.data.get("tool_call_id"),
                "status": "running",
            }
        elif event.type == "tool.result":
            failed = bool(event.data.get("is_error", False))
            self.last_action = {
                "tool": event.data.get("tool"),
                "tool_call_id": event.data.get("tool_call_id"),
                "status": "failed" if failed else "succeeded",
            }
            if failed:
                self.failure_count += 1
                self.no_progress_count += 1
            else:
                self.no_progress_count = 0
        elif event.type in {"progress.updated", "capability.acquired", "goal.completed"}:
            self.no_progress_count = 0

    def record_submission(self, *, accepted: bool, completed: bool) -> None:
        if accepted:
            self.accepted_submissions += 1
            self.no_progress_count = 0
        else:
            self.rejected_submissions += 1
            self.failure_count += 1
            self.no_progress_count += 1
        if completed:
            self.objective_completed = True

    def write(self, path: Path) -> None:
        path.write_text(
            json.dumps(self.model_dump(mode="json"), ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
