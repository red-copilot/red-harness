import json
from pathlib import Path

from harness.budget import BudgetMonitor
from harness.models import BudgetSpec
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
