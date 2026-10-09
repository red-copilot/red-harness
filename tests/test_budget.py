import json
import os
import sys
from pathlib import Path

import pytest

from harness.budget import BudgetMonitor
from harness.models import BudgetSpec, load_task
from harness.trace import TraceRecorder


def test_budget_monitor_accounts_and_exceeds(tmp_path: Path) -> None:
    events = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test")
    monitor = BudgetMonitor(
        events,
        BudgetSpec(max_tokens=10, max_model_calls=2, max_tool_calls=2),
        trace,
    )
    events.write_text(
        json.dumps(
            {
                "type": "model.usage",
                "data": {"input_tokens": 8, "output_tokens": 4, "cost_usd": 0.2, "model_calls": 1},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "max_tokens"
    assert monitor.metrics.total_tokens == 12
    assert monitor.metrics.model_calls == 1


def test_budget_monitor_fails_closed_on_malformed_or_negative_untrusted_usage(
    tmp_path: Path,
) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        "\n".join(
            (
                json.dumps({"type": "model.usage", "data": {"total_tokens": 12}}),
                json.dumps({"type": "model.usage", "data": {"total_tokens": -100}}),
            )
        )
        + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "invalid_telemetry"

    assert monitor.metrics.total_tokens == 12
    assert monitor.metrics.model_calls == 0
    assert monitor.metrics.tool_calls == 0
    trace_events = [json.loads(line) for line in trace.path.read_text().splitlines()]
    assert any(event["type"] == "agent.telemetry_error" for event in trace_events)


def test_budget_monitor_fails_closed_on_malformed_usage_counters(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        json.dumps({"type": "model.request", "data": {"count": "many"}}) + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.model_calls == 0


def test_budget_monitor_rejects_total_tokens_below_input_output_sum(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(max_tokens=5), trace)
    event_file.write_text(
        json.dumps(
            {
                "type": "model.usage",
                "data": {
                    "input_tokens": 6,
                    "output_tokens": 5,
                    "total_tokens": 1,
                    "cost_usd": 0.01,
                },
            }
        )
        + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 0


def test_budget_monitor_fails_closed_on_missing_model_usage(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        json.dumps({"type": "model.usage_missing", "data": {"source": "pi"}}) + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "invalid_telemetry"


@pytest.mark.parametrize(
    "malformed_record",
    [
        "{not-json",
        "[]",
        json.dumps({"type": "model.usage", "data": []}),
        '{"type":"other","data":{"value":NaN}}',
    ],
)
def test_budget_monitor_fails_closed_on_malformed_telemetry_records(
    tmp_path: Path, malformed_record: str
) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 7}})
        + "\n"
        + malformed_record
        + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 7
    trace_events = [json.loads(line) for line in trace.path.read_text().splitlines()]
    assert any(
        event["type"] == "agent.telemetry_error"
        and "record is malformed" in event["data"]["message"]
        for event in trace_events
    )


def test_budget_monitor_fails_closed_on_negative_tool_counter(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        json.dumps({"type": "tool.call", "data": {"count": -5}}) + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.tool_calls == 0


def test_budget_monitor_fails_closed_if_agent_truncates_telemetry_after_poll(
    tmp_path: Path,
) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 7}}) + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() is None
    assert monitor.metrics.total_tokens == 7

    event_file.write_text("", encoding="utf-8")

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 7
    trace_events = [json.loads(line) for line in trace.path.read_text().splitlines()]
    assert any(
        event["type"] == "agent.telemetry_error"
        and "replaced or truncated" in event["data"]["message"]
        for event in trace_events
    )


def test_budget_monitor_fails_closed_if_agent_replaces_telemetry_file(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    event_file.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 7}}) + "\n",
        encoding="utf-8",
    )

    assert monitor.poll() is None
    replacement = tmp_path / "replacement.jsonl"
    replacement.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 9}}) + "\n",
        encoding="utf-8",
    )
    replacement.replace(event_file)

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 7


@pytest.mark.skipif(not hasattr(os, "O_NOFOLLOW"), reason="no-follow file access is required")
def test_budget_monitor_fails_closed_if_agent_replaces_telemetry_with_symlink(
    tmp_path: Path,
) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    event_file.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 7}}) + "\n",
        encoding="utf-8",
    )
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    assert monitor.poll() is None

    event_file.unlink()
    outside = tmp_path / "outside.jsonl"
    outside.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 100}}) + "\n",
        encoding="utf-8",
    )
    event_file.symlink_to(outside)

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 7


def test_budget_monitor_fails_closed_if_agent_removes_telemetry_file(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    event_file.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 7}}) + "\n",
        encoding="utf-8",
    )
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    assert monitor.poll() is None
    event_file.unlink()

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 7


@pytest.mark.skipif(not hasattr(os, "O_NOFOLLOW"), reason="no-follow file access is required")
def test_supervisor_stops_agent_that_replaces_telemetry_with_symlink(tmp_path: Path) -> None:
    from harness.agent import _run_monitored

    event_file = tmp_path / "events.jsonl"
    outside = tmp_path / "outside.jsonl"
    outside.write_text("{}\n", encoding="utf-8")
    script = (
        "from pathlib import Path; import time; "
        f"event=Path({str(event_file)!r}); outside=Path({str(outside)!r}); "
        "event.unlink(); event.symlink_to(outside); time.sleep(30)"
    )
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    result = _run_monitored(
        [sys.executable, "-c", script],
        cwd=tmp_path,
        env={"PATH": os.environ.get("PATH", "")},
        run_dir=tmp_path,
        task=load_task(Path("benchmarks/examples/hello/task.yaml")),
        trace=trace,
    )

    assert result.budget_exceeded == "invalid_telemetry"
    assert result.returncode != 0


def test_budget_monitor_rejects_oversized_telemetry_file_before_reading(
    tmp_path: Path, monkeypatch
) -> None:
    import harness.budget as budget_module

    event_file = tmp_path / "events.jsonl"
    event_file.write_bytes(b"x" * 17)
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    monkeypatch.setattr(budget_module, "MAX_AGENT_TELEMETRY_BYTES", 16)

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 0


def test_budget_monitor_rejects_oversized_unterminated_record(
    tmp_path: Path, monkeypatch
) -> None:
    import harness.budget as budget_module

    event_file = tmp_path / "events.jsonl"
    event_file.write_bytes(b"x" * 17)
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    monkeypatch.setattr(budget_module, "MAX_AGENT_TELEMETRY_EVENT_BYTES", 16)

    assert monitor.poll() == "invalid_telemetry"
    assert monitor.metrics.total_tokens == 0


def test_budget_monitor_streams_telemetry_in_bounded_chunks(
    tmp_path: Path, monkeypatch
) -> None:
    import harness.budget as budget_module

    event_file = tmp_path / "events.jsonl"
    event_file.write_text(
        json.dumps({"type": "model.usage", "data": {"total_tokens": 12}}) + "\n",
        encoding="utf-8",
    )
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-budget", "task-budget")
    monitor = BudgetMonitor(event_file, BudgetSpec(), trace)
    monkeypatch.setattr(budget_module, "MAX_AGENT_TELEMETRY_CHUNK_BYTES", 16)

    for _ in range(10):
        monitor.poll()
        if monitor.metrics.total_tokens:
            break

    assert monitor.metrics.total_tokens == 12
