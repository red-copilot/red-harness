from __future__ import annotations

import hashlib
import json
import math
import os
from datetime import datetime
from pathlib import Path
from typing import Any

from .secureio import open_regular_file

_TRACE_REQUIRED_FIELDS = {
    "event_id",
    "parent_event_id",
    "plan_id",
    "subgoal_id",
    "action_id",
    "hypothesis_id",
    "ts",
    "run_id",
    "task_id",
    "type",
    "actor",
    "data",
}
_TRACE_STRING_FIELDS = ("event_id", "ts", "run_id", "task_id", "type", "actor")
_TRACE_OPTIONAL_ID_FIELDS = (
    "parent_event_id",
    "plan_id",
    "subgoal_id",
    "action_id",
    "hypothesis_id",
)
MAX_OTLP_INPUT_BYTES = 64 * 1024 * 1024
MAX_OTLP_EVENT_BYTES = 256 * 1024
MAX_OTLP_SPANS = 10_000
MAX_OTLP_RESPONSE_BYTES = 4 * 1024 * 1024


def _unix_nano(ts: str) -> int:
    parsed = datetime.fromisoformat(ts)
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise ValueError("trace timestamps must include a timezone")
    return int(parsed.timestamp() * 1_000_000_000)


def _reject_non_finite(value: str) -> None:
    raise ValueError(f"invalid JSON constant: {value}")


def _finite_float(value: str) -> float:
    parsed = float(value)
    if not math.isfinite(parsed):
        raise ValueError("trace contains a non-finite number")
    return parsed


def _validate_trace_event(event: dict[str, Any]) -> None:
    if not _TRACE_REQUIRED_FIELDS.issubset(event):
        raise ValueError("trace event is missing required fields")
    if event.keys() - _TRACE_REQUIRED_FIELDS:
        raise ValueError("trace event has unexpected fields")
    if any(not isinstance(event[field], str) or not event[field] for field in _TRACE_STRING_FIELDS):
        raise ValueError("trace event has an invalid string field")
    if any(
        event[field] is not None and not isinstance(event[field], str)
        for field in _TRACE_OPTIONAL_ID_FIELDS
    ):
        raise ValueError("trace event has an invalid identifier field")
    if not isinstance(event["data"], dict):
        raise TypeError("trace event data must be an object")
    _unix_nano(event["ts"])


def _value(value: Any) -> dict[str, Any]:
    if isinstance(value, bool):
        return {"boolValue": value}
    if isinstance(value, int):
        return {"intValue": str(value)}
    if isinstance(value, float):
        return {"doubleValue": value}
    if value is None:
        return {"stringValue": "null"}
    if isinstance(value, (dict, list)):
        return {"stringValue": json.dumps(value, ensure_ascii=False, separators=(",", ":"))}
    return {"stringValue": str(value)}


def _attributes(event: dict[str, Any]) -> list[dict[str, Any]]:
    attrs: dict[str, Any] = {
        "harness.run_id": event.get("run_id"),
        "harness.task_id": event.get("task_id"),
        "harness.actor": event.get("actor"),
        "harness.event_type": event.get("type"),
    }
    data = event.get("data", {})
    if isinstance(data, dict):
        for key, value in data.items():
            attrs[f"harness.data.{key}"] = value
    return [{"key": key, "value": _value(value)} for key, value in attrs.items()]


def trace_to_otlp_json(trace_path: str | Path) -> dict[str, Any]:
    path = Path(trace_path)
    spans: list[dict[str, Any]] = []
    encoded_spans_bytes = 0
    run_id: str | None = None
    with open_regular_file(path, "rb") as handle:
        size = os.fstat(handle.fileno()).st_size
        if size > MAX_OTLP_INPUT_BYTES:
            raise OSError("trace exceeds the OTLP input limit")
        line_number = 0
        bytes_read = 0
        while raw_line := handle.readline(MAX_OTLP_EVENT_BYTES + 2):
            bytes_read += len(raw_line)
            if bytes_read > MAX_OTLP_INPUT_BYTES:
                raise OSError("trace grew beyond the OTLP input limit")
            line_number += 1
            if len(raw_line) > MAX_OTLP_EVENT_BYTES + 1:
                raise ValueError(f"trace event at line {line_number} exceeds the size limit")
            if not raw_line.strip():
                continue
            try:
                event = json.loads(
                    raw_line,
                    parse_constant=_reject_non_finite,
                    parse_float=_finite_float,
                )
            except (json.JSONDecodeError, ValueError) as exc:
                raise ValueError(f"invalid trace JSON at line {line_number}") from exc
            if not isinstance(event, dict):
                raise TypeError(f"trace event at line {line_number} must be an object")
            try:
                _validate_trace_event(event)
            except (TypeError, ValueError) as exc:
                raise ValueError(f"invalid trace event at line {line_number}") from exc
            if len(spans) >= MAX_OTLP_SPANS:
                raise ValueError("OTLP response exceeds the span count limit")
            if run_id is None:
                run_id = str(event["run_id"])
            trace_id = hashlib.sha256(run_id.encode()).hexdigest()[:32]
            start = _unix_nano(event["ts"])
            span_id = hashlib.sha256(f"{run_id}:{len(spans) + 1}".encode()).hexdigest()[:16]
            span = {
                "traceId": trace_id,
                "spanId": span_id,
                "name": event["type"],
                "kind": 1,
                "startTimeUnixNano": str(start),
                "endTimeUnixNano": str(start + 1),
                "attributes": _attributes(event),
                "status": {"code": 1},
            }
            span_bytes = len(
                json.dumps(span, ensure_ascii=True, separators=(",", ":")).encode("utf-8")
            )
            encoded_spans_bytes += span_bytes + (1 if spans else 0)
            if encoded_spans_bytes > MAX_OTLP_RESPONSE_BYTES:
                raise ValueError("OTLP response exceeds the size limit")
            spans.append(span)

    if not spans:
        return {"resourceSpans": []}
    payload = {
        "resourceSpans": [
            {
                "resource": {
                    "attributes": [
                        {"key": "service.name", "value": {"stringValue": "harness"}},
                        {"key": "harness.run_id", "value": {"stringValue": run_id}},
                    ]
                },
                "scopeSpans": [
                    {
                        "scope": {"name": "harness.trace", "version": "0.6.0"},
                        "spans": spans,
                    }
                ],
            }
        ]
    }
    if len(json.dumps(payload, ensure_ascii=True, separators=(",", ":")).encode("utf-8")) > MAX_OTLP_RESPONSE_BYTES:
        raise ValueError("OTLP response exceeds the size limit")
    return payload


def export_otlp_json(trace_path: str | Path, output_path: str | Path) -> Path:
    output = Path(output_path)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(trace_to_otlp_json(trace_path), ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return output
