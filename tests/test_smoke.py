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



def test_smoke_resume_world_state(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    orchestrator = Orchestrator(tmp_path / "runs")

    first = orchestrator.run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=1,
    )
    first_dir = tmp_path / "runs" / first["run_id"]
    world_events = first_dir / "world.events.jsonl"
    world_context = first_dir / "world.context.txt"

    assert world_events.is_file()
    assert world_context.is_file()
    assert "Red Harness World Context" in world_context.read_text(encoding="utf-8")

    second = orchestrator.run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=2,
        resume_world_events=world_events,
    )

    assert second["world"]["resumed"] is True
    assert second["world"]["revision"] >= first["world"]["revision"]
