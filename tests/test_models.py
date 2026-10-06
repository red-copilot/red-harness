from pathlib import Path

from redharness.models import load_agent, load_task


def test_example_contracts_are_valid() -> None:
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    agent = load_agent(Path("agents/examples/demo.yaml"))
    assert task.api_version == "redharness/v1"
    assert task.budgets.wall_time == 30
    assert agent.type == "cli"
