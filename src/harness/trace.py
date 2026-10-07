from __future__ import annotations

import json
import threading
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


class TraceRecorder:
    def __init__(self, path: Path, run_id: str, task_id: str) -> None:
        self.path = path
        self.run_id = run_id
        self.task_id = task_id
        self._lock = threading.Lock()
        self.path.parent.mkdir(parents=True, exist_ok=True)

    def emit(
        self,
        event_type: str,
        *,
        actor: str = "harness",
        data: dict[str, Any] | None = None,
        event_id: str | None = None,
        parent_event_id: str | None = None,
        plan_id: str | None = None,
        subgoal_id: str | None = None,
        action_id: str | None = None,
        hypothesis_id: str | None = None,
    ) -> str:
        emitted_id = event_id or f"evt_{uuid.uuid4().hex}"
        event = {
            "event_id": emitted_id,
            "parent_event_id": parent_event_id,
            "plan_id": plan_id,
            "subgoal_id": subgoal_id,
            "action_id": action_id,
            "hypothesis_id": hypothesis_id,
            "ts": datetime.now(UTC).isoformat(),
            "run_id": self.run_id,
            "task_id": self.task_id,
            "type": event_type,
            "actor": actor,
            "data": data or {},
        }
        line = json.dumps(event, ensure_ascii=False, separators=(",", ":"))
        with self._lock, self.path.open("a", encoding="utf-8") as handle:
            handle.write(line + "\n")
        return emitted_id
