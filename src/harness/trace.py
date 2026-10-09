from __future__ import annotations

import hashlib
import json
import re
import threading
import uuid
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

MAX_TRACE_EVENT_BYTES = 256 * 1024
MAX_TRACE_STRING_CHARS = 16 * 1024
_SENSITIVE_KEY = re.compile(
    r"(?:^|[_-])(?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|"
    r"password|passwd|secret|credential|cookie|token)(?:$|[_-])",
    re.IGNORECASE,
)
_ASSIGNMENT_SECRET = re.compile(
    r"(?i)\b(?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|"
    r"password|passwd|client[_-]?secret|secret|credential|cookie|token)\s*[:=]\s*"
    r"(?:Bearer\s+)?[^\s,;\"']+"
)
_BEARER = re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+")
_EMAIL = re.compile(r"\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b", re.IGNORECASE)


def _redact_string(value: str) -> str:
    value = _ASSIGNMENT_SECRET.sub("[REDACTED]", value)
    value = _BEARER.sub("Bearer [REDACTED]", value)
    value = _EMAIL.sub("[REDACTED_EMAIL]", value)
    if len(value) > MAX_TRACE_STRING_CHARS:
        value = value[:MAX_TRACE_STRING_CHARS] + "…[TRUNCATED]"
    return value


def _sanitize(value: Any, *, key: str | None = None, depth: int = 0) -> Any:
    if key is not None and _SENSITIVE_KEY.search(key):
        return "[REDACTED]"
    if isinstance(value, str):
        return _redact_string(value)
    if isinstance(value, dict):
        if depth >= 8:
            return "[MAX_DEPTH]"
        items = list(value.items())
        safe = {
            str(item_key): _sanitize(item_value, key=str(item_key), depth=depth + 1)
            for item_key, item_value in items[:100]
        }
        if len(items) > 100:
            safe["_truncated_fields"] = len(items) - 100
        return safe
    if isinstance(value, (list, tuple)):
        if depth >= 8:
            return "[MAX_DEPTH]"
        items = list(value)
        safe = [_sanitize(item, depth=depth + 1) for item in items[:100]]
        if len(items) > 100:
            safe.append(f"[TRUNCATED {len(items) - 100} ITEMS]")
        return safe
    if value is None or isinstance(value, (bool, int, float)):
        return value
    return _redact_string(str(value))


def sanitize_observability_data(data: dict[str, Any]) -> dict[str, Any]:
    """Redact secrets and bound user-controlled values before report export."""
    safe = _sanitize(data)
    return safe if isinstance(safe, dict) else {}


class TraceRecorder:
    def __init__(self, path: Path, run_id: str, task_id: str) -> None:
        self.path = path
        self.run_id = _redact_string(run_id)
        self.task_id = _redact_string(task_id)
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
        emitted_id = _redact_string(event_id or f"evt_{uuid.uuid4().hex}")
        event = {
            "event_id": emitted_id,
            "parent_event_id": _redact_string(parent_event_id) if parent_event_id else None,
            "plan_id": _redact_string(plan_id) if plan_id else None,
            "subgoal_id": _redact_string(subgoal_id) if subgoal_id else None,
            "action_id": _redact_string(action_id) if action_id else None,
            "hypothesis_id": _redact_string(hypothesis_id) if hypothesis_id else None,
            "ts": datetime.now(UTC).isoformat(),
            "run_id": self.run_id,
            "task_id": self.task_id,
            "type": _redact_string(event_type),
            "actor": _redact_string(actor),
            "data": _sanitize(data or {}),
        }
        line = json.dumps(event, ensure_ascii=False, separators=(",", ":"))
        if len(line.encode("utf-8")) > MAX_TRACE_EVENT_BYTES:
            event["data"] = {
                "truncated": True,
                "sanitized_data_sha256": hashlib.sha256(
                    json.dumps(event["data"], ensure_ascii=False, sort_keys=True).encode("utf-8")
                ).hexdigest(),
            }
            line = json.dumps(event, ensure_ascii=False, separators=(",", ":"))
        with self._lock, self.path.open("a", encoding="utf-8") as handle:
            handle.write(line + "\n")
        return emitted_id
