"""Read-only consistency audit for completed Harness run artifacts."""

from __future__ import annotations

import hashlib
import json
import math
import os
import stat
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, Field

SCHEMA_VERSION = "harness/audit/v1"
MAX_INPUT_BYTES = 64 * 1024 * 1024
MAX_TRACE_EVENTS = 100_000
_REQUIRED = ("result.json", "trace.jsonl")
_OPTIONAL = ("progress.json", "world.events.jsonl", "world.snapshot.json")


class AuditIssue(BaseModel):
    code: str
    severity: Literal["error", "warning"]
    file: str
    line: int | None = None
    field: str | None = None
    event_id: str | None = None


class AuditReport(BaseModel):
    schema_version: str = SCHEMA_VERSION
    run_id: str | None = None
    status: Literal["consistent", "contradictory", "incomplete"]
    inputs: dict[str, dict[str, Any]] = Field(default_factory=dict)
    counts: dict[str, int] = Field(default_factory=dict)
    issues: list[AuditIssue] = Field(default_factory=list)
    statement: str = "Consistency only; source labels do not prove authenticity."


class _AuditInputError(Exception):
    def __init__(self, code: str, file: str) -> None:
        self.code = code
        self.file = file


def _add(
    issues: list[AuditIssue],
    code: str,
    severity: str,
    file: str,
    *,
    line: int | None = None,
    field: str | None = None,
    event_id: str | None = None,
) -> None:
    if not isinstance(event_id, str):
        event_id = None
    issues.append(
        AuditIssue(
            code=code, severity=severity, file=file, line=line, field=field, event_id=event_id
        )
    )


def _read(path: Path, name: str, remaining: int) -> tuple[bytes, dict[str, Any]]:
    try:
        fd = os.open(
            path,
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0),
        )
    except FileNotFoundError as exc:
        raise _AuditInputError("missing_required_file", name) from exc
    except OSError as exc:
        raise _AuditInputError("unsafe_or_invalid_input", name) from exc
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode) or before.st_size > remaining:
            raise _AuditInputError(
                "input_size_limit_exceeded"
                if before.st_size > remaining
                else "unsafe_or_invalid_input",
                name,
            )
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = os.read(fd, min(1024 * 1024, remaining - total + 1))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > remaining:
                raise _AuditInputError("input_size_limit_exceeded", name)
        data = b"".join(chunks)
        after = os.fstat(fd)
    except OSError as exc:
        raise _AuditInputError("input_read_error", name) from exc
    finally:
        os.close(fd)
    changed = (before.st_size, before.st_mtime_ns, before.st_ctime_ns, before.st_ino) != (
        after.st_size,
        after.st_mtime_ns,
        after.st_ctime_ns,
        after.st_ino,
    ) or len(data) != after.st_size
    return data, {
        "size_bytes": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
        "changed_during_read": changed,
    }


def _json(raw: bytes, file: str, issues: list[AuditIssue]) -> dict[str, Any] | None:
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        _add(issues, "invalid_json", "warning", file)
        return None
    if not isinstance(value, dict):
        _add(issues, "invalid_json_shape", "warning", file)
        return None
    return value


def _jsonl(
    raw: bytes,
    file: str,
    issues: list[AuditIssue],
    *,
    max_events: int | None = None,
) -> list[tuple[int, dict[str, Any]]]:
    events: list[tuple[int, dict[str, Any]]] = []
    lines = raw.splitlines()
    for number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            value = json.loads(line)
        except (UnicodeDecodeError, json.JSONDecodeError):
            code = "truncated_event" if number == len(lines) else "invalid_event_json"
            _add(issues, code, "warning", file, line=number)
            continue
        if not isinstance(value, dict):
            _add(issues, "invalid_event_shape", "warning", file, line=number)
            continue
        if max_events is not None and len(events) >= max_events:
            _add(issues, "event_count_limit_exceeded", "warning", file, line=number)
            break
        events.append((number, value))
    return events


def _same(a: Any, b: Any) -> bool:
    if (
        isinstance(a, (int, float))
        and not isinstance(a, bool)
        and isinstance(b, (int, float))
        and not isinstance(b, bool)
    ):
        return math.isclose(float(a), float(b), rel_tol=1e-9, abs_tol=1e-9)
    return a == b


def _add_if_diff(
    issues: list[AuditIssue],
    line: int,
    event: dict[str, Any],
    event_key: str,
    result: dict[str, Any],
    result_key: str,
) -> None:
    data = event.get("data")
    if not isinstance(data, dict) or event_key not in data or result_key not in result:
        return
    if not _same(data[event_key], result[result_key]):
        _add(
            issues,
            "terminal_result_mismatch",
            "error",
            "trace.jsonl",
            line=line,
            field=result_key,
            event_id=event.get("event_id"),
        )


def audit_run(run_dir: str | Path) -> AuditReport:
    """Audit fixed run artifacts without modifying them or following references."""
    root = Path(run_dir)
    issues: list[AuditIssue] = []
    inputs: dict[str, dict[str, Any]] = {}
    raw_files: dict[str, bytes] = {}
    total = 0
    try:
        if not root.is_dir():
            raise _AuditInputError("run_directory_missing", ".")
        for name in (*_REQUIRED, *_OPTIONAL):
            path = root / name
            if name in _OPTIONAL and not path.exists() and not path.is_symlink():
                continue
            try:
                raw, summary = _read(path, name, MAX_INPUT_BYTES - total)
            except _AuditInputError as exc:
                _add(issues, exc.code, "warning", exc.file)
                continue
            raw_files[name] = raw
            inputs[name] = summary
            total += len(raw)
            if summary["changed_during_read"]:
                _add(issues, "input_changed_during_read", "warning", name)
    except OSError:
        _add(issues, "run_directory_read_error", "warning", ".")
        return AuditReport(
            run_id=root.name or None, status="incomplete", inputs=inputs, issues=issues
        )

    result = (
        _json(raw_files["result.json"], "result.json", issues)
        if "result.json" in raw_files
        else None
    )
    trace = (
        _jsonl(raw_files["trace.jsonl"], "trace.jsonl", issues, max_events=MAX_TRACE_EVENTS)
        if "trace.jsonl" in raw_files
        else []
    )
    unique: list[tuple[int, dict[str, Any]]] = []
    seen: dict[str, dict[str, Any]] = {}
    for line, event in trace:
        event_id = event.get("event_id")
        if not isinstance(event_id, str) or not event_id:
            _add(issues, "event_id_missing", "warning", "trace.jsonl", line=line)
            unique.append((line, event))
            continue
        previous = seen.get(event_id)
        if previous is None:
            seen[event_id] = event
            unique.append((line, event))
        elif previous == event:
            _add(issues, "duplicate_event", "warning", "trace.jsonl", line=line, event_id=event_id)
        else:
            _add(issues, "event_id_conflict", "error", "trace.jsonl", line=line, event_id=event_id)
            unique.append((line, event))

    counts = {
        "claims": 0,
        "verified": 0,
        "contradicted": 0,
        "pending": 0,
        "untraceable_verdicts": 0,
        "events": len(unique),
    }
    terminal: list[tuple[int, dict[str, Any]]] = []
    grader: list[tuple[int, dict[str, Any]]] = []
    action_ids: set[str] = set()
    for _, event in unique:
        if event.get("type") == "action.verified":
            continue
        action_id = event.get("action_id")
        if isinstance(action_id, str) and action_id:
            action_ids.add(action_id)
        data = event.get("data")
        if isinstance(data, dict) and isinstance(data.get("tool_call_id"), str):
            action_ids.add(data["tool_call_id"])
    for line, event in unique:
        kind = event.get("type")
        data = event.get("data") if isinstance(event.get("data"), dict) else {}
        if kind == "progress.updated" and isinstance(data.get("confirmed_fact"), str):
            counts["claims"] += 1
            _add(
                issues,
                "claim_without_linked_evidence",
                "warning",
                "trace.jsonl",
                line=line,
                field="data.confirmed_fact",
                event_id=event.get("event_id"),
            )
        if (
            kind in {"goal.completed", "objective.completed"}
            or data.get("objective_completed") is True
        ):
            counts["claims"] += 1
            _add(
                issues,
                "completion_claim",
                "warning",
                "trace.jsonl",
                line=line,
                field="data.objective_completed",
                event_id=event.get("event_id"),
            )
        if kind == "tool.result" and data.get("is_error") is False:
            counts["claims"] += 1
            _add(
                issues,
                "claim_without_linked_evidence",
                "warning",
                "trace.jsonl",
                line=line,
                field="data.is_error",
                event_id=event.get("event_id"),
            )
        if kind == "action.verified":
            status = data.get("status")
            if status in {"verified", "contradicted", "pending"}:
                counts[status] += 1
                action_id = event.get("action_id") or data.get("action_id")
                if not isinstance(action_id, str) or action_id not in action_ids:
                    counts["untraceable_verdicts"] += 1
                    _add(
                        issues,
                        "verdict_untraceable",
                        "warning",
                        "trace.jsonl",
                        line=line,
                        field="action_id",
                        event_id=event.get("event_id"),
                    )
            else:
                _add(
                    issues,
                    "verification_status_unknown",
                    "warning",
                    "trace.jsonl",
                    line=line,
                    field="data.status",
                    event_id=event.get("event_id"),
                )
        elif kind == "run.finished":
            terminal.append((line, event))
        elif kind == "grader.result":
            grader.append((line, event))

    if result is not None:
        version = result.get("schema_version")
        if version not in (None, "harness/result/v1"):
            _add(
                issues,
                "unsupported_result_version",
                "warning",
                "result.json",
                field="schema_version",
            )
        if not terminal:
            _add(issues, "terminal_event_missing", "warning", "trace.jsonl")
        for line, event in terminal:
            for key in ("status", "success", "score"):
                data = event.get("data")
                if not isinstance(data, dict) or key not in data:
                    _add(
                        issues,
                        "terminal_event_field_missing",
                        "warning",
                        "trace.jsonl",
                        line=line,
                        field=f"data.{key}",
                        event_id=event.get("event_id"),
                    )
                    continue
                _add_if_diff(issues, line, event, key, result, key)
        for line, event in grader:
            for key in ("success", "score"):
                data = event.get("data")
                if not isinstance(data, dict) or key not in data:
                    _add(
                        issues,
                        "grader_event_field_missing",
                        "warning",
                        "trace.jsonl",
                        line=line,
                        field=f"data.{key}",
                        event_id=event.get("event_id"),
                    )
                    continue
                _add_if_diff(issues, line, event, key, result, key)
        if any(key not in result for key in ("success", "status", "score")):
            _add(issues, "result_fields_missing", "warning", "result.json")

    if "progress.json" in raw_files:
        progress = _json(raw_files["progress.json"], "progress.json", issues)
        if progress is not None and progress.get("schema_version") not in (
            None,
            "harness/progress/v1",
            "harness/progress/v2",
        ):
            _add(
                issues,
                "unsupported_progress_version",
                "warning",
                "progress.json",
                field="schema_version",
            )
        if (
            progress is not None
            and result is not None
            and isinstance(progress.get("objective_completed"), bool)
            and progress["objective_completed"] != result.get("success")
        ):
            _add(
                issues,
                "completion_state_mismatch",
                "error",
                "progress.json",
                field="objective_completed",
            )

    if "world.events.jsonl" in raw_files:
        world_events = _jsonl(
            raw_files["world.events.jsonl"],
            "world.events.jsonl",
            issues,
            max_events=MAX_TRACE_EVENTS,
        )
        for line, event in world_events:
            version = event.get("schema_version")
            if version not in (None, "harness/world/v1"):
                _add(
                    issues,
                    "unsupported_world_event_version",
                    "warning",
                    "world.events.jsonl",
                    line=line,
                    event_id=event.get("id"),
                )
            obj = event.get("object")
            if (
                event.get("kind") == "goal"
                and isinstance(obj, dict)
                and obj.get("status") == "completed"
                and str(event.get("actor", "")).startswith("agent:")
            ):
                counts["claims"] += 1
                _add(
                    issues,
                    "completion_claim",
                    "warning",
                    "world.events.jsonl",
                    line=line,
                    field="object.status",
                    event_id=event.get("id"),
                )

    if "world.snapshot.json" in raw_files:
        snapshot = _json(raw_files["world.snapshot.json"], "world.snapshot.json", issues)
        if snapshot is not None and snapshot.get("schema_version") not in (
            None,
            "harness/world/v1",
        ):
            _add(
                issues,
                "unsupported_world_snapshot_version",
                "warning",
                "world.snapshot.json",
                field="schema_version",
            )
        if snapshot is not None and result is not None:
            goals = snapshot.get("goals", {})
            roots = (
                [g for g in goals.values() if isinstance(g, dict) and g.get("parent_id") is None]
                if isinstance(goals, dict)
                else []
            )
            completed = any(g.get("status") == "completed" for g in roots)
            active_or_failed = any(g.get("status") in {"active", "failed"} for g in roots)
            if (
                result.get("success") is True
                and active_or_failed
                or result.get("success") is False
                and completed
            ):
                _add(
                    issues,
                    "world_goal_result_mismatch",
                    "error",
                    "world.snapshot.json",
                    field="goals",
                )

    if any(i.severity == "error" for i in issues):
        status = "contradictory"
    elif result is None or any(name not in raw_files for name in _REQUIRED) or issues:
        status = "incomplete"
    else:
        status = "consistent"
    return AuditReport(
        run_id=(
            (result or {}).get("run_id")
            if isinstance((result or {}).get("run_id"), str)
            else root.name
        ),
        status=status,
        inputs=inputs,
        counts=counts,
        issues=issues,
    )
