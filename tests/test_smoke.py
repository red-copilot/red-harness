from pathlib import Path

from redharness.models import load_agent, load_task
from redharness.orchestrator import Orchestrator


def test_smoke_run(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=42,
    )
    assert result["status"] == "finished"
    assert result["success"] is True
    assert result["score"] == 100
    assert result["seed"] == 42
    assert result["metrics"]["total_tokens"] == 12
    assert result["metrics"]["model_calls"] == 1
    assert result["metrics"]["tool_calls"] == 1
    assert result["metrics"]["cost_usd"] == 0.01
