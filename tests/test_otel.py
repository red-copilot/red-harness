import json
from pathlib import Path

import pytest

from harness.otel import export_otlp_json, trace_to_otlp_json


def test_trace_to_otlp_json(tmp_path: Path) -> None:
    trace = tmp_path / "trace.jsonl"
    trace.write_text(
        json.dumps(
            {
                "ts": "2026-10-06T08:00:00+00:00",
                "event_id": "evt_1",
                "parent_event_id": None,
                "plan_id": None,
                "subgoal_id": None,
                "action_id": None,
                "hypothesis_id": None,
                "run_id": "run_abc",
                "task_id": "task_1",
                "type": "tool.call",
                "actor": "agent",
                "data": {"tool": "file.read", "count": 1},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    payload = trace_to_otlp_json(trace)
    resource = payload["resourceSpans"][0]
    span = resource["scopeSpans"][0]["spans"][0]
    assert span["name"] == "tool.call"
    assert len(span["traceId"]) == 32
    assert len(span["spanId"]) == 16

    output = export_otlp_json(trace, tmp_path / "otel.json")
    assert output.is_file()
    assert json.loads(output.read_text(encoding="utf-8"))["resourceSpans"]


def test_trace_to_otlp_rejects_symlink_and_malformed_input(tmp_path: Path) -> None:
    outside = tmp_path / "outside.jsonl"
    outside.write_text("{}\n", encoding="utf-8")
    link = tmp_path / "trace-link.jsonl"
    link.symlink_to(outside)
    with pytest.raises(OSError):
        trace_to_otlp_json(link)

    malformed = tmp_path / "malformed.jsonl"
    malformed.write_text("{broken\n", encoding="utf-8")
    with pytest.raises(ValueError, match="invalid trace JSON"):
        trace_to_otlp_json(malformed)

    non_finite = tmp_path / "non-finite.jsonl"
    non_finite.write_text(
        '{"event_id":"e","parent_event_id":null,"plan_id":null,'
        '"subgoal_id":null,"action_id":null,"hypothesis_id":null,'
        '"ts":"2026-10-06T08:00:00+00:00","run_id":"r","task_id":"t",'
        '"type":"event","actor":"harness","data":{"count":1e999}}\n',
        encoding="utf-8",
    )
    with pytest.raises(ValueError, match="invalid trace JSON"):
        trace_to_otlp_json(non_finite)


def test_trace_to_otlp_rejects_expanded_output_over_limit(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import harness.otel as otel_module

    trace = tmp_path / "trace.jsonl"
    trace.write_text(
        json.dumps(
            {
                "ts": "2026-10-06T08:00:00+00:00",
                "event_id": "evt_1",
                "parent_event_id": None,
                "plan_id": None,
                "subgoal_id": None,
                "action_id": None,
                "hypothesis_id": None,
                "run_id": "run_abc",
                "task_id": "task_1",
                "type": "tool.call",
                "actor": "agent",
                "data": {"tool": "file.read", "count": 1},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(otel_module, "MAX_OTLP_RESPONSE_BYTES", 100, raising=False)

    with pytest.raises(ValueError, match="OTLP response exceeds"):
        trace_to_otlp_json(trace)
