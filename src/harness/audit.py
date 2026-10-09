"""Read-only consistency audit for completed Harness run artifacts."""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import stat
from pathlib import Path, PurePosixPath
from typing import Any, Literal

from pydantic import BaseModel, Field

SCHEMA_VERSION = "harness/audit/v1"
MAX_INPUT_BYTES = 64 * 1024 * 1024
MAX_TRACE_EVENTS = 100_000
MAX_JSONL_LINE_BYTES = 256 * 1024
MAX_EVIDENCE_BYTES = 1024 * 1024
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
    line_limit = (max_events if max_events is not None else MAX_TRACE_EVENTS) + 1
    offset = 0
    number = 0
    while offset < len(raw):
        line_start = offset
        newline = raw.find(b"\n", offset)
        end = len(raw) if newline < 0 else newline
        content_end = end - 1 if end > offset and raw[end - 1] == 13 else end
        offset = len(raw) if newline < 0 else newline + 1
        number += 1
        if number > line_limit:
            _add(issues, "line_count_limit_exceeded", "warning", file, line=number)
            break
        if content_end - line_start > MAX_JSONL_LINE_BYTES:
            _add(issues, "event_size_limit_exceeded", "warning", file, line=number)
            break
        line = raw[line_start:content_end]
        if not line.strip():
            continue
        try:
            value = json.loads(
                line,
                parse_constant=_reject_json_constant,
                parse_float=_finite_float,
            )
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
            code = "truncated_event" if offset >= len(raw) else "invalid_event_json"
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


def _finite_float(value: str) -> float:
    parsed = float(value)
    if not math.isfinite(parsed):
        raise ValueError("non-finite JSON number")
    return parsed


def _reject_json_constant(value: str) -> None:
    raise ValueError(f"non-finite JSON constant: {value}")


def _same(a: Any, b: Any) -> bool:
    if (
        isinstance(a, (int, float))
        and not isinstance(a, bool)
        and isinstance(b, (int, float))
        and not isinstance(b, bool)
    ):
        return math.isclose(float(a), float(b), rel_tol=1e-9, abs_tol=1e-9)
    return a == b


def _read_evidence_artifact(root: Path, reference: str) -> bytes:
    pure_path = PurePosixPath(reference)
    if (
        not reference
        or pure_path.is_absolute()
        or any(part in {"", ".", ".."} for part in pure_path.parts)
        or "\\" in reference
        or "\x00" in reference
    ):
        raise ValueError("unsafe evidence artifact reference")
    directory_flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    directory_fd = os.open(root, directory_flags)
    try:
        for component in pure_path.parts[:-1]:
            next_fd = os.open(component, directory_flags, dir_fd=directory_fd)
            os.close(directory_fd)
            directory_fd = next_fd
        file_fd = os.open(
            pure_path.parts[-1],
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0),
            dir_fd=directory_fd,
        )
        try:
            info = os.fstat(file_fd)
            if not stat.S_ISREG(info.st_mode) or info.st_size > MAX_EVIDENCE_BYTES:
                raise ValueError("unsafe or oversized evidence artifact")
            chunks: list[bytes] = []
            remaining = MAX_EVIDENCE_BYTES + 1
            while remaining > 0:
                chunk = os.read(file_fd, min(64 * 1024, remaining))
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            data = b"".join(chunks)
            if len(data) > MAX_EVIDENCE_BYTES or len(data) != info.st_size:
                raise ValueError("evidence artifact size changed or exceeded limit")
            return data
        finally:
            os.close(file_fd)
    finally:
        os.close(directory_fd)


def _validate_objective_evidence(
    root: Path,
    event: dict[str, Any],
    data: dict[str, Any],
    result: dict[str, Any] | None,
    issues: list[AuditIssue],
    line: int,
) -> tuple[str | None, bool]:
    from pydantic import ValidationError

    from .verification import ObjectiveVerdict

    try:
        verdict = ObjectiveVerdict.model_validate(data)
    except (ValidationError, TypeError, ValueError):
        _add(issues, "objective_verdict_invalid", "error", "trace.jsonl", line=line)
        return None, False
    if event.get("actor") != "harness":
        _add(
            issues,
            "objective_verdict_untrusted_actor",
            "error",
            "trace.jsonl",
            line=line,
            event_id=event.get("event_id"),
        )
        return verdict.status, False
    expected_run_id = result.get("run_id") if isinstance(result, dict) else event.get("run_id")
    if isinstance(expected_run_id, str) and verdict.run_id != expected_run_id:
        _add(
            issues,
            "objective_verdict_run_mismatch",
            "error",
            "trace.jsonl",
            line=line,
            field="data.run_id",
            event_id=event.get("event_id"),
        )
        return verdict.status, False

    valid = True
    for evidence in verdict.evidence:
        if not re.fullmatch(r"[0-9a-f]{64}", evidence.artifact_hash or ""):
            valid = False
            _add(issues, "objective_evidence_hash_invalid", "error", "trace.jsonl", line=line)
            continue
        if evidence.evidence_id != f"sha256:{evidence.artifact_hash}":
            valid = False
            _add(issues, "objective_evidence_id_mismatch", "error", "trace.jsonl", line=line)
            continue
        if evidence.artifact_ref != f"evidence/sha256/{evidence.artifact_hash}.json":
            valid = False
            _add(
                issues,
                "objective_evidence_reference_mismatch",
                "error",
                "trace.jsonl",
                line=line,
            )
            continue
        try:
            artifact = _read_evidence_artifact(root, evidence.artifact_ref or "")
        except (OSError, ValueError):
            valid = False
            _add(issues, "objective_evidence_missing_or_unsafe", "error", "trace.jsonl", line=line)
            continue
        digest = hashlib.sha256(artifact).hexdigest()
        if digest != evidence.artifact_hash:
            valid = False
            _add(issues, "objective_evidence_hash_mismatch", "error", "trace.jsonl", line=line)
            continue
        try:
            artifact_data = json.loads(artifact)
        except (UnicodeDecodeError, json.JSONDecodeError):
            valid = False
            _add(issues, "objective_evidence_invalid", "error", "trace.jsonl", line=line)
            continue
        if not isinstance(artifact_data, dict) or any(
            artifact_data.get(key) != expected
            for key, expected in (
                ("schema_version", "harness/objective-evidence/v1"),
                ("run_id", verdict.run_id),
                ("action_id", evidence.action_id),
                ("producer", verdict.producer),
                ("captured_at", verdict.captured_at.isoformat()),
                ("world_revision", verdict.world_revision),
                ("status", verdict.status),
            )
        ):
            valid = False
            _add(
                issues, "objective_evidence_provenance_mismatch", "error", "trace.jsonl", line=line
            )
    return verdict.status, valid and bool(verdict.evidence)


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
        "objective_verdicts": 0,
        "events": len(unique),
    }
    terminal: list[tuple[int, dict[str, Any]]] = []
    grader: list[tuple[int, dict[str, Any]]] = []
    objective_verdicts: list[tuple[int, str | None, dict[str, Any], bool]] = []
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
        if kind == "objective.verdict":
            counts["objective_verdicts"] += 1
            objective_status, evidence_valid = _validate_objective_evidence(
                root,
                event,
                data,
                result,
                issues,
                line,
            )
            objective_verdicts.append((line, objective_status, data, evidence_valid))
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
                evidence = data.get("evidence")
                if status == "verified" and (
                    not isinstance(evidence, list)
                    or not evidence
                    or any(
                        not isinstance(item, dict)
                        or not isinstance(item.get("evidence_id"), str)
                        or not item.get("evidence_id")
                        for item in evidence
                    )
                ):
                    _add(
                        issues,
                        "verdict_evidence_missing",
                        "error",
                        "trace.jsonl",
                        line=line,
                        field="data.evidence",
                        event_id=event.get("event_id"),
                    )
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
        if version not in (None, "harness/result/v1", "harness/result/v2"):
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
        if version == "harness/result/v2" and result.get("success") is True:
            terminal_line = max((line for line, _ in terminal), default=-1)
            preceding_verdicts = [
                verdict for verdict in objective_verdicts if verdict[0] < terminal_line
            ]
            revisions = [
                revision
                for _, _, data, _ in preceding_verdicts
                if isinstance((revision := data.get("world_revision")), int)
                and not isinstance(revision, bool)
                and revision >= 0
            ]
            latest_revision = max(revisions, default=-1)
            current_verdicts = [
                verdict
                for verdict in preceding_verdicts
                if verdict[2].get("world_revision") == latest_revision
            ]
            latest_verdict = max(current_verdicts, key=lambda verdict: verdict[0], default=None)
            for line, _, data, _ in preceding_verdicts:
                revision = data.get("world_revision")
                if isinstance(revision, int) and not isinstance(revision, bool) and revision < latest_revision:
                    _add(
                        issues,
                        "objective_verdict_superseded",
                        "warning",
                        "trace.jsonl",
                        line=line,
                    )
            has_verified_objective = (
                latest_verdict is not None and latest_verdict[1] == "verified" and latest_verdict[3]
            )
            if len({verdict[1] for verdict in current_verdicts if verdict[1]}) > 1:
                _add(
                    issues,
                    "objective_verdict_conflict",
                    "error",
                    "trace.jsonl",
                )
            if not has_verified_objective:
                _add(
                    issues,
                    "objective_verdict_missing_or_untraceable",
                    "error",
                    "trace.jsonl",
                )
            result_progress = result.get("progress")
            projected_verdict = (
                result_progress.get("objective_verdict")
                if isinstance(result_progress, dict)
                else None
            )
            if not (
                latest_verdict is not None
                and projected_verdict == latest_verdict[2]
                and latest_verdict[1] == "verified"
                and latest_verdict[3]
            ):
                _add(
                    issues,
                    "objective_verdict_projection_mismatch",
                    "error",
                    "result.json",
                    field="progress.objective_verdict",
                )

    if "progress.json" in raw_files:
        progress = _json(raw_files["progress.json"], "progress.json", issues)
        if progress is not None and progress.get("schema_version") not in (
            None,
            "harness/progress/v1",
            "harness/progress/v2",
            "harness/progress/v3",
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
