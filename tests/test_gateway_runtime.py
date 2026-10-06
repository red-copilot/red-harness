import json
from pathlib import Path

from redharness.gateway_runtime import GatewayConfig
from redharness.models import load_agent, load_task
from redharness.orchestrator import Orchestrator


def test_per_run_gateway_is_injected_and_accounted(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/gateway-demo.yaml")

    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=9,
        gateway_config=GatewayConfig(
            policy_path=Path("examples/gateway-policy.yaml").resolve(),
        ),
    )

    assert result["status"] == "finished"
    assert result["success"] is True
    assert result["gateway"] == {
        "enabled": True,
        "model_proxy": False,
        "docker_access": False,
    }
    assert result["metrics"]["tool_calls"] == 1

    run_dir = next((tmp_path / "runs").iterdir())
    trace = [
        json.loads(line)
        for line in (run_dir / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    event_types = [event["type"] for event in trace]
    assert "gateway.started" in event_types
    assert "tool.call" in event_types
    assert "tool.result" in event_types
    assert "gateway.finished" in event_types
