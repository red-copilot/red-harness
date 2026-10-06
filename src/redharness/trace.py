from __future__ import annotations

import json
import threading
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

    def emit(self, event_type: str, *, actor: str = "harness", data: dict[str, Any] | None = None) -> None:
        event = {
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
