import json
from pathlib import Path

from harness.otel import export_otlp_json, trace_to_otlp_json


def test_trace_to_otlp_json(tmp_path: Path) -> None:
    trace = tmp_path / "trace.jsonl"
    trace.write_text(
        json.dumps(
            {
                "ts": "2026-10-06T08:00:00+00:00",
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
