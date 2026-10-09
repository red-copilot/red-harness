from __future__ import annotations

import json
from pathlib import Path

from harness.trace import MAX_TRACE_EVENT_BYTES, TraceRecorder


def test_trace_redacts_secret_fields_bearer_values_and_email(tmp_path) -> None:
    trace_path = tmp_path / "trace.jsonl"
    trace = TraceRecorder(trace_path, "run-trace", "task-trace")

    trace.emit(
        "agent.output",
        data={
            "api_key": "openai-secret",
            "token": "generic-token-secret",
            "nested": {"access_token": "access-secret"},
            "message": "Authorization: Bearer bearer-secret; contact a.person@example.com",
        },
    )

    raw = trace_path.read_text(encoding="utf-8")
    event = json.loads(raw)
    schema = json.loads(Path("schemas/trace-event.schema.json").read_text(encoding="utf-8"))
    assert "openai-secret" not in raw
    assert "generic-token-secret" not in raw
    assert "access-secret" not in raw
    assert "bearer-secret" not in raw
    assert "a.person@example.com" not in raw
    assert event["data"]["api_key"] == "[REDACTED]"
    assert event["data"]["token"] == "[REDACTED]"
    assert event["data"]["nested"]["access_token"] == "[REDACTED]"
    assert event["event_id"]
    assert event["run_id"] == "run-trace"
    assert set(schema["required"]) == set(event)
    assert schema["additionalProperties"] is False
    assert schema["properties"]["data"]["type"] == "object"


def test_trace_bounds_oversized_event_payloads(tmp_path) -> None:
    trace_path = tmp_path / "trace.jsonl"
    trace = TraceRecorder(trace_path, "run-large", "task-large")

    trace.emit("large.output", data={f"field-{index}": "x" * 12_000 for index in range(100)})

    line = trace_path.read_text(encoding="utf-8").strip()
    event = json.loads(line)
    assert len(line.encode("utf-8")) <= MAX_TRACE_EVENT_BYTES
    assert event["data"]["truncated"] is True
    assert len(event["data"]["sanitized_data_sha256"]) == 64


def test_trace_redacts_sensitive_identifiers_and_top_level_fields(tmp_path) -> None:
    trace_path = tmp_path / "trace.jsonl"
    trace = TraceRecorder(
        trace_path,
        "run Authorization: Bearer run-secret-canary",
        "task owner@example.com",
    )
    emitted_id = trace.emit(
        "Authorization: Bearer event-type-secret",
        actor="actor@example.com",
        event_id="api_key=event-id-secret",
        parent_event_id="Bearer parent-id-secret",
        plan_id="access_token=plan-secret",
        subgoal_id="subgoal@example.com",
        action_id="password=action-secret",
        hypothesis_id="cookie=hypothesis-secret",
    )

    raw = trace_path.read_text(encoding="utf-8")
    event = json.loads(raw)

    for canary in (
        "run-secret-canary",
        "owner@example.com",
        "event-type-secret",
        "actor@example.com",
        "event-id-secret",
        "parent-id-secret",
        "plan-secret",
        "subgoal@example.com",
        "action-secret",
        "hypothesis-secret",
    ):
        assert canary not in raw
    assert emitted_id == event["event_id"]
