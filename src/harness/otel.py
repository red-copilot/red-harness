from __future__ import annotations

import hashlib
import json
from datetime import datetime
from pathlib import Path
from typing import Any


def _unix_nano(ts: str) -> int:
    return int(datetime.fromisoformat(ts).timestamp() * 1_000_000_000)


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
    events = [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]
    if not events:
        return {"resourceSpans": []}

    run_id = str(events[0].get("run_id", "unknown"))
    trace_id = hashlib.sha256(run_id.encode()).hexdigest()[:32]
    spans: list[dict[str, Any]] = []
    for index, event in enumerate(events, start=1):
        start = _unix_nano(str(event["ts"]))
        span_id = hashlib.sha256(f"{run_id}:{index}".encode()).hexdigest()[:16]
        spans.append(
            {
                "traceId": trace_id,
                "spanId": span_id,
                "name": str(event.get("type", "harness.event")),
                "kind": 1,
                "startTimeUnixNano": str(start),
                "endTimeUnixNano": str(start + 1),
                "attributes": _attributes(event),
                "status": {"code": 1},
            }
        )

    return {
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


def export_otlp_json(trace_path: str | Path, output_path: str | Path) -> Path:
    output = Path(output_path)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(trace_to_otlp_json(trace_path), ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return output
