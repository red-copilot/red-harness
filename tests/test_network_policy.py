from __future__ import annotations

import subprocess

import pytest

from harness.agent import AgentError
from harness.models import AgentSpec
from harness.network_policy import EnforcedNetworks, enforced_network


def _network_id(name: str) -> str:
    return ("a" if name.startswith("gateway") else "b") * 64


def _spec(profile: str, *, agent_type: str = "docker") -> AgentSpec:
    payload = {
        "apiVersion": "harness/v1",
        "id": "agent",
        "type": agent_type,
        "network_profile": profile,
    }
    if agent_type == "docker":
        payload["image"] = "agent:test"
    elif agent_type == "pi":
        payload["image"] = "agent:test"
        payload["pi"] = {"model": "model"}
    else:
        payload["command"] = ["python", "agent.py"]
    return AgentSpec.model_validate(payload)


def test_target_only_requires_and_uses_internal_environment_network(monkeypatch) -> None:
    calls = []

    def inspect(command, **kwargs):
        calls.append(command)
        return subprocess.CompletedProcess(
            command, 0, stdout=f"{_network_id(command[-1])} true\n", stderr=""
        )

    monkeypatch.setattr("harness.network_policy.subprocess.run", inspect)

    network = enforced_network(
        _spec("target-only"),
        environment_network="task_internal",
        gateway_network=None,
        gateway_enabled=False,
    )

    assert network == EnforcedNetworks(
        primary=_network_id("task_internal"), environment=_network_id("task_internal")
    )
    assert calls == [
        ["docker", "network", "inspect", "--format", "{{.Id}} {{.Internal}}", "task_internal"]
    ]


def test_target_only_fails_closed_on_non_internal_network(monkeypatch) -> None:
    monkeypatch.setattr(
        "harness.network_policy.subprocess.run",
        lambda command, **kwargs: subprocess.CompletedProcess(
            command, 0, stdout=f"{_network_id(command[-1])} false\n", stderr=""
        ),
    )

    with pytest.raises(AgentError, match="Internal=true"):
        enforced_network(
            _spec("target-only"),
            environment_network="public_bridge",
            gateway_network=None,
            gateway_enabled=False,
        )


def test_strict_network_rejects_malformed_inspect_result(monkeypatch) -> None:
    monkeypatch.setattr(
        "harness.network_policy.subprocess.run",
        lambda command, **kwargs: subprocess.CompletedProcess(
            command, 0, stdout="not-a-docker-network-id true\n", stderr=""
        ),
    )

    with pytest.raises(AgentError, match="Internal=true"):
        enforced_network(
            _spec("target-only"),
            environment_network="task_internal",
            gateway_network=None,
            gateway_enabled=False,
        )


def test_model_allowed_requires_gateway_and_internal_target_network(monkeypatch) -> None:
    inspected = []

    def inspect(command, **kwargs):
        inspected.append(command[-1])
        return subprocess.CompletedProcess(
            command, 0, stdout=f"{_network_id(command[-1])} true\n", stderr=""
        )

    monkeypatch.setattr("harness.network_policy.subprocess.run", inspect)

    network = enforced_network(
        _spec("model-allowed"),
        environment_network="task_internal",
        gateway_network="gateway_internal",
        gateway_enabled=True,
    )

    assert network == EnforcedNetworks(
        primary=_network_id("gateway_internal"),
        environment=_network_id("task_internal"),
        gateway=_network_id("gateway_internal"),
    )
    assert inspected == ["gateway_internal", "task_internal"]


def test_model_allowed_rejects_non_internal_gateway_network(monkeypatch) -> None:
    def inspect(command, **kwargs):
        network = command[-1]
        stdout = f"{_network_id(network)} {'false' if network == 'gateway_public' else 'true'}\n"
        return subprocess.CompletedProcess(command, 0, stdout=stdout, stderr="")

    monkeypatch.setattr("harness.network_policy.subprocess.run", inspect)

    with pytest.raises(AgentError, match="gateway_public.*Internal=true"):
        enforced_network(
            _spec("model-allowed"),
            environment_network="task_internal",
            gateway_network="gateway_public",
            gateway_enabled=True,
        )


def test_model_allowed_without_sidecar_and_strict_host_agent_fail_closed() -> None:
    with pytest.raises(AgentError, match="sidecar model Gateway"):
        enforced_network(
            _spec("model-allowed"),
            environment_network=None,
            gateway_network=None,
            gateway_enabled=True,
        )

    with pytest.raises(AgentError, match="requires a Docker Agent"):
        enforced_network(
            _spec("target-only", agent_type="cli"),
            environment_network="task_internal",
            gateway_network=None,
            gateway_enabled=False,
        )


def test_fully_offline_uses_no_network_and_rejects_gateway() -> None:
    assert (
        enforced_network(
            _spec("fully-offline"),
            environment_network="task_internal",
            gateway_network=None,
            gateway_enabled=False,
        )
        == EnforcedNetworks(primary="none")
    )
    with pytest.raises(AgentError, match="incompatible with Gateway"):
        enforced_network(
            _spec("fully-offline"),
            environment_network=None,
            gateway_network="gateway_internal",
            gateway_enabled=True,
        )


def test_legacy_benchmark_only_is_explicitly_advisory() -> None:
    assert (
        enforced_network(
            _spec("benchmark-only"),
            environment_network="public_bridge",
            gateway_network=None,
            gateway_enabled=False,
        )
        is None
    )


def test_docker_adapter_uses_enforced_target_network(monkeypatch, tmp_path) -> None:
    import harness.agent as agent_module
    from harness.agent import AgentResult, DockerAdapter
    from harness.budget import UsageMetrics
    from harness.models import TaskSpec
    from harness.trace import TraceRecorder

    monkeypatch.setattr(agent_module.shutil, "which", lambda _name: "/usr/bin/docker")
    spec = _spec("target-only")
    adapter = DockerAdapter(
        spec,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task"),
    )
    task = TaskSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "task",
            "name": "Task",
            "objective": {"description": "exercise target network"},
        }
    )
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    (run_dir / "agent-workspace").mkdir()
    create_commands = []

    def fake_subprocess_run(command, **kwargs):
        return subprocess.CompletedProcess(
            command, 0, stdout=f"{_network_id(command[-1])} true\n", stderr=""
        )

    monkeypatch.setattr("harness.network_policy.subprocess.run", fake_subprocess_run)
    monkeypatch.setattr(
        adapter,
        "_docker",
        lambda command, **kwargs: create_commands.append(command)
        or subprocess.CompletedProcess(command, 0, stdout="", stderr=""),
    )
    monkeypatch.setattr(
        agent_module,
        "_run_monitored",
        lambda *args, **kwargs: AgentResult(0, False, None, "", "", UsageMetrics()),
    )

    adapter.run(
        task,
        task_dir=tmp_path,
        run_dir=run_dir / "agent-workspace",
        environment_project="task",
        environment_network="task_internal",
        seed=1,
    )

    create = create_commands[0]
    assert create[create.index("--network") + 1] == _network_id("task_internal")


def test_docker_adapter_uses_verified_ids_for_model_allowed_networks(monkeypatch, tmp_path) -> None:
    import harness.agent as agent_module
    from harness.agent import AgentResult, DockerAdapter
    from harness.budget import UsageMetrics
    from harness.models import TaskSpec
    from harness.trace import TraceRecorder

    monkeypatch.setattr(agent_module.shutil, "which", lambda _name: "/usr/bin/docker")
    spec = _spec("model-allowed")
    adapter = DockerAdapter(spec, trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task"))
    task = TaskSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "task",
            "name": "Task",
            "objective": {"description": "exercise isolated model and target networks"},
        }
    )
    run_dir = tmp_path / "run"
    workspace = run_dir / "agent-workspace"
    workspace.mkdir(parents=True)
    commands = []

    def fake_run(command, **kwargs):
        return subprocess.CompletedProcess(
            command, 0, stdout=f"{_network_id(command[-1])} true\n", stderr=""
        )

    monkeypatch.setattr("harness.network_policy.subprocess.run", fake_run)
    monkeypatch.setattr(
        adapter,
        "_docker",
        lambda command, **kwargs: commands.append(command)
        or subprocess.CompletedProcess(command, 0, stdout="", stderr=""),
    )
    monkeypatch.setattr(
        agent_module,
        "_run_monitored",
        lambda *args, **kwargs: AgentResult(0, False, None, "", "", UsageMetrics()),
    )

    adapter.run(
        task,
        task_dir=tmp_path,
        run_dir=workspace,
        environment_project="task",
        environment_network="task_internal",
        gateway_network="gateway_internal",
        gateway_url="http://harness-gateway:8765",
        gateway_token="one-time-token",
        seed=1,
    )

    create = commands[0]
    assert create[create.index("--network") + 1] == _network_id("gateway_internal")
    assert commands[1] == [
        "docker",
        "network",
        "connect",
        _network_id("task_internal"),
        create[create.index("--name") + 1],
    ]
