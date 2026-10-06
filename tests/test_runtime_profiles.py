from pathlib import Path

from redharness.agent import DockerAdapter
from redharness.models import AgentSpec, TaskSpec
from redharness.trace import TraceRecorder
from redharness.verifier import build_docker_verifier_command


def test_docker_agent_runtime_flag(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("redharness.agent.shutil.which", lambda _: "/usr/bin/docker")
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "redharness/v1",
            "id": "gvisor-agent",
            "type": "docker",
            "image": "agent:test",
            "runtime": "runsc",
        }
    )
    adapter = DockerAdapter(
        spec,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )
    task = TaskSpec.model_validate(
        {
            "apiVersion": "redharness/v1",
            "id": "task",
            "name": "Task",
            "objective": {"description": "test"},
        }
    )
    command = adapter._create_command(
        task,
        task_dir=tmp_path,
        run_dir=tmp_path,
        container_name="agent",
        network="none",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )
    assert command[command.index("--runtime") + 1] == "runsc"


def test_docker_verifier_runtime_flag(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("redharness.verifier.shutil.which", lambda _: "/usr/bin/docker")
    task = TaskSpec.model_validate(
        {
            "apiVersion": "redharness/v1",
            "id": "task",
            "name": "Task",
            "objective": {"description": "test"},
            "verification": {
                "type": "docker",
                "image": "verifier:test",
                "runtime": "runsc",
            },
        }
    )
    command = build_docker_verifier_command(
        task,
        task_dir=tmp_path,
        run_dir=tmp_path,
        environment_project=None,
        environment_network=None,
        seed=1,
    )
    assert command[command.index("--runtime") + 1] == "runsc"
