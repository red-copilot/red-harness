from pathlib import Path

import pytest
from typer.testing import CliRunner

from harness.cli import app
from harness.models import load_agent, load_task
from harness.preflight import run_preflight


def test_preflight_checks_local_cli_task_and_reports_network_policy(monkeypatch) -> None:
    monkeypatch.setattr("harness.preflight.shutil.which", lambda command: "/usr/bin/python")
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")

    report = run_preflight(load_task(task_path), load_agent(agent_path), task_path=task_path)

    assert report.ready
    assert report.by_name("task_input").ok
    assert report.by_name("verifier").ok
    assert "network_profile=unrestricted" in report.by_name("network_policy").detail
    assert "no connectivity probe performed" in report.by_name("network_policy").detail


def test_preflight_fails_early_for_missing_cli_tool_and_verifier(
    tmp_path: Path, monkeypatch
) -> None:
    task_path = tmp_path / "task.yaml"
    task_path.write_text(
        "apiVersion: harness/v1\nid: t\nname: t\n"
        "objective:\n  description: solve\n"
        "verification:\n  type: python\n  entrypoint: missing.py\n",
        encoding="utf-8",
    )
    agent_path = Path("agents/examples/demo.yaml")
    monkeypatch.setattr("harness.preflight.shutil.which", lambda _command: None)

    report = run_preflight(load_task(task_path), load_agent(agent_path), task_path=task_path)

    assert not report.ready
    assert not report.by_name("agent_command").ok
    assert not report.by_name("verifier").ok


def test_container_preflight_rejects_hardlinked_agent_task_input(
    tmp_path: Path, monkeypatch
) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    task_path = task_dir / "task.yaml"
    task_path.write_text(
        "apiVersion: harness/v1\nid: t\nname: t\n"
        "objective:\n  description: solve\n"
        "verification:\n  type: python\n  entrypoint: verifier.py\n",
        encoding="utf-8",
    )
    verifier = task_dir / "verifier.py"
    verifier.write_text("def verify(_context): return True\n", encoding="utf-8")
    (task_dir / "challenge.bin").hardlink_to(verifier)
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/docker.yaml"))
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)

    report = run_preflight(task, agent, task_path=task_path)

    assert not report.ready
    assert not report.by_name("agent_task_inputs").ok
    assert "hard-linked" in report.by_name("agent_task_inputs").detail
    assert not (task_dir / "agent-task-input").exists()


def test_container_preflight_rejects_symlinked_verifier_before_view_allocation(
    tmp_path: Path, monkeypatch
) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    task_path = task_dir / "task.yaml"
    task_path.write_text(
        "apiVersion: harness/v1\nid: t\nname: t\n"
        "objective:\n  description: solve\n"
        "verification:\n  type: python\n  entrypoint: verifier.py\n",
        encoding="utf-8",
    )
    (task_dir / "grader.py").write_text(
        "def verify(_context): return True\n", encoding="utf-8"
    )
    (task_dir / "verifier.py").symlink_to(task_dir / "grader.py")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/docker.yaml"))
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)

    report = run_preflight(task, agent, task_path=task_path)

    assert not report.ready
    assert not report.by_name("agent_task_inputs").ok
    assert "symlinks" in report.by_name("agent_task_inputs").detail
    assert not (task_dir / "agent-task-input").exists()


def test_preflight_cli_reports_local_readiness(monkeypatch) -> None:
    monkeypatch.setattr("harness.preflight.shutil.which", lambda command: "/usr/bin/python")
    result = CliRunner().invoke(
        app,
        [
            "preflight",
            "benchmarks/examples/hello/task.yaml",
            "--agent",
            "agents/examples/demo.yaml",
        ],
    )
    assert result.exit_code == 0, result.output
    assert '"ready": true' in result.output


def test_tsec_preflight_checks_sdk_and_credentials_without_disclosing_values(
    monkeypatch,
) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    monkeypatch.setattr("harness.preflight.shutil.which", lambda _command: "/usr/bin/python")
    monkeypatch.setattr("harness.preflight.find_spec", lambda _name: object())
    monkeypatch.setenv("BENCHMARK_BASE_URL", "https://benchmark.invalid")
    monkeypatch.setenv("BENCHMARK_TOKEN", "private-test-token")

    report = run_preflight(
        load_task(task_path),
        load_agent(agent_path),
        task_path=task_path,
        require_tsec=True,
    )

    assert report.by_name("tsec_sdk").ok
    assert report.by_name("tsec_base_url").ok
    assert report.by_name("tsec_credential").ok
    assert "private-test-token" not in report.model_dump_json()


def test_tsec_preflight_fails_when_sdk_or_credentials_are_missing(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    monkeypatch.setattr("harness.preflight.shutil.which", lambda _command: "/usr/bin/python")
    monkeypatch.setattr("harness.preflight.find_spec", lambda _name: None)
    monkeypatch.delenv("BENCHMARK_BASE_URL", raising=False)
    monkeypatch.delenv("BENCHMARK_TOKEN", raising=False)

    report = run_preflight(
        load_task(task_path),
        load_agent(agent_path),
        task_path=task_path,
        require_tsec=True,
    )

    assert not report.ready
    assert not report.by_name("tsec_sdk").ok
    assert not report.by_name("tsec_base_url").ok
    assert not report.by_name("tsec_credential").ok


def test_offline_network_preflight_reports_enforced_network_override(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/demo.yaml")).model_copy(
        update={"network_profile": "offline", "network": "host"}
    )
    monkeypatch.setattr("harness.preflight.shutil.which", lambda _command: "/usr/bin/python")

    report = run_preflight(task, agent, task_path=task_path)

    detail = report.by_name("network_policy").detail
    assert "configured_network=host" in detail
    assert "effective_agent_network=none" in detail


def test_preflight_rejects_strict_network_profile_for_host_agent(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/demo.yaml")).model_copy(
        update={"network_profile": "target-only"}
    )
    monkeypatch.setattr("harness.preflight.shutil.which", lambda _command: "/usr/bin/python")

    report = run_preflight(task, agent, task_path=task_path)

    check = report.by_name("network_policy")
    assert not check.ok
    assert "require a Docker Agent" in check.detail


def test_preflight_rejects_target_only_without_environment_network(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/docker.yaml")).model_copy(
        update={"network_profile": "target-only"}
    )
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)

    report = run_preflight(task, agent, task_path=task_path)

    assert not report.ready
    assert "requires a Docker Compose environment network" in report.by_name(
        "network_policy"
    ).detail


def test_preflight_requires_and_checks_sidecar_for_model_allowed(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/docker.yaml")).model_copy(
        update={"network_profile": "model-allowed"}
    )
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)

    missing = run_preflight(task, agent, task_path=task_path)
    assert not missing.ready
    assert "requires --gateway" in missing.by_name("network_policy").detail

    ready = run_preflight(
        task,
        agent,
        task_path=task_path,
        gateway_enabled=True,
        gateway_mode="sidecar",
        gateway_image="harness-gateway:test",
        model_upstream="https://model.example/v1",
    )
    assert ready.ready
    assert ready.by_name("gateway_image").ok


def test_preflight_requires_local_sidecar_image_for_any_sidecar_gateway(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/docker.yaml"))
    monkeypatch.setattr(
        "harness.preflight._local_image_available",
        lambda image: image != "harness-gateway:test",
    )

    report = run_preflight(
        task,
        agent,
        task_path=task_path,
        gateway_enabled=True,
        gateway_mode="sidecar",
        gateway_image="harness-gateway:test",
    )

    assert not report.ready
    assert not report.by_name("gateway_image").ok


def test_pi_gateway_preflight_uses_gateway_model_route_not_direct_provider_key(
    monkeypatch,
) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/pi.yaml"))
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)

    report = run_preflight(
        task,
        agent,
        task_path=task_path,
        gateway_enabled=True,
        gateway_mode="host",
        model_upstream="https://model.example/v1",
    )

    assert report.ready
    assert not any(check.name == "model_credential" for check in report.checks)


@pytest.mark.parametrize("credential_source", ["agent_env", "host_env"])
def test_pi_preflight_rejects_empty_provider_credentials(
    monkeypatch, credential_source: str
) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/pi.yaml"))
    if credential_source == "agent_env":
        agent.env["OPENAI_API_KEY"] = ""
        agent.pi.env_passthrough = []
        monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    else:
        monkeypatch.setenv("OPENAI_API_KEY", "")
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)

    report = run_preflight(task, agent, task_path=task_path)

    assert not report.ready
    assert not report.by_name("model_credential").ok


def test_pi_gateway_preflight_requires_a_model_upstream(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/pi.yaml"))
    monkeypatch.setattr("harness.preflight._local_image_available", lambda _image: True)

    report = run_preflight(
        task,
        agent,
        task_path=task_path,
        gateway_enabled=True,
        gateway_mode="host",
    )

    assert not report.ready
    assert "Pi Gateway mode requires --model-upstream" in report.by_name(
        "network_policy"
    ).detail


def test_preflight_warns_legacy_benchmark_only_is_advisory(monkeypatch) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent = load_agent(Path("agents/examples/demo.yaml")).model_copy(
        update={"network_profile": "benchmark-only"}
    )
    monkeypatch.setattr("harness.preflight.shutil.which", lambda _command: "/usr/bin/python")

    report = run_preflight(task, agent, task_path=task_path)

    check = report.by_name("network_policy")
    assert check.ok
    assert "advisory" in check.detail
    assert "does not isolate public egress" in check.detail
