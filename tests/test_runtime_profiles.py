from pathlib import Path

import pytest

from harness.agent import AgentError, DockerAdapter
from harness.models import AgentSpec, TaskSpec
from harness.trace import TraceRecorder
from harness.verifier import build_docker_verifier_command


def test_docker_agent_runtime_flag(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("harness.agent.shutil.which", lambda _: "/usr/bin/docker")
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
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
            "apiVersion": "harness/v1",
            "id": "task",
            "name": "Task",
            "objective": {"description": "test"},
        }
    )
    protected_run_dir = tmp_path / "run"
    protected_run_dir.mkdir()
    workspace = protected_run_dir / "agent-workspace"
    workspace.mkdir()
    command = adapter._create_command(
        task,
        task_dir=tmp_path,
        run_dir=workspace,
        container_name="agent",
        network="none",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )
    assert command[command.index("--runtime") + 1] == "runsc"
    assert command[command.index("--user") + 1] == (
        f"{workspace.stat().st_uid}:{workspace.stat().st_gid}"
    )
    assert command[command.index("--cap-drop") + 1] == "ALL"
    assert "no-new-privileges" in command
    assert command[command.index("--pids-limit") + 1] == "256"
    assert command[command.index("--memory") + 1] == "2g"
    assert command[command.index("--cpus") + 1] == "2"
    assert "/tmp:rw,nosuid,nodev,size=256m" in command
    assert f"{workspace.resolve()}:/run/harness:rw" in command
    assert f"{protected_run_dir.resolve()}:/run/harness:rw" not in command
    task_mount = next(value for value in command if value.endswith(":/task:ro"))
    assert task_mount != f"{tmp_path.resolve()}:/task:ro"
    assert not (protected_run_dir / "agent-task-input" / "verifier.py").exists()
    assert not (protected_run_dir / "agent-task-input" / "agent-workspace").exists()


def test_strict_docker_profile_clears_docker_cli_proxy_defaults(
    monkeypatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("harness.agent.shutil.which", lambda _name: "/usr/bin/docker")
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "strict-agent",
            "type": "docker",
            "image": "agent:test",
            "network_profile": "target-only",
        }
    )
    adapter = DockerAdapter(
        spec, trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task")
    )
    task = TaskSpec.model_validate(
        {"apiVersion": "harness/v1", "id": "task", "name": "Task", "objective": {"description": "test"}}
    )
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    command = adapter._create_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        container_name="strict-agent",
        network="internal-task",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )

    for name in (
        "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "FTP_PROXY", "ALL_PROXY",
        "http_proxy", "https_proxy", "no_proxy", "ftp_proxy", "all_proxy",
    ):
        assert f"{name}=" in command


def test_strict_docker_profile_rejects_agent_configured_proxy(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("harness.agent.shutil.which", lambda _name: "/usr/bin/docker")
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "strict-agent",
            "type": "docker",
            "image": "agent:test",
            "network_profile": "model-allowed",
            "env": {"HTTPS_PROXY": "http://proxy.invalid"},
        }
    )
    adapter = DockerAdapter(
        spec, trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task")
    )
    task = TaskSpec.model_validate(
        {"apiVersion": "harness/v1", "id": "task", "name": "Task", "objective": {"description": "test"}}
    )
    run_dir = tmp_path / "run"
    run_dir.mkdir()

    with pytest.raises(AgentError, match="strict network profile.*proxy"):
        adapter._create_command(
            task,
            task_dir=Path("benchmarks/examples/hello").resolve(),
            run_dir=run_dir,
            container_name="strict-agent",
            network="gateway-internal",
            environment_project=None,
            seed=1,
            gateway_url="http://harness-gateway:8765",
            gateway_token="gateway-token",
            host_gateway=False,
        )


def test_docker_launch_rechecks_reserved_environment_names(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("harness.agent.shutil.which", lambda _: "/usr/bin/docker")
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1", "id": "container", "type": "docker",
            "image": "agent:test",
        }
    )
    spec.env["HARNESS_CONTROL_TOKEN"] = "secret"
    adapter = DockerAdapter(
        spec, trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task")
    )
    task = TaskSpec.model_validate(
        {"apiVersion": "harness/v1", "id": "task", "name": "Task", "objective": {"description": "test"}}
    )
    run_dir = tmp_path / "run"
    run_dir.mkdir()

    with pytest.raises(AgentError, match="reserved credential environment"):
        adapter._create_command(
            task,
            task_dir=tmp_path,
            run_dir=run_dir,
            container_name="agent",
            network="none",
            environment_project=None,
            seed=1,
            gateway_url=None,
            gateway_token=None,
            host_gateway=False,
        )


def test_docker_verifier_runtime_flag(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("harness.verifier.shutil.which", lambda _: "/usr/bin/docker")
    task = TaskSpec.model_validate(
        {
            "apiVersion": "harness/v1",
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


def test_network_projection_reports_strict_profiles() -> None:
    from harness.runtime.projections import network_result

    target = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "target",
            "type": "docker",
            "image": "agent:test",
            "network_profile": "target-only",
        }
    )
    model = target.model_copy(update={"network_profile": "model-allowed"})
    offline = target.model_copy(update={"network_profile": "fully-offline"})

    assert network_result(target)["enforcement"] == "docker-internal-network"
    assert network_result(model)["enforcement"] == "docker-internal-network-gateway-proxy"
    assert network_result(offline)["enforcement"] == "docker-none"
