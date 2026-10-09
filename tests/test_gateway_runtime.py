import json
from pathlib import Path

from fastapi.testclient import TestClient

from harness.gateway import create_gateway_app
from harness.gateway_runtime import DockerGatewaySidecarRuntime, GatewayConfig
from harness.models import AgentSpec, load_agent, load_task
from harness.orchestrator import Orchestrator
from harness.trace import TraceRecorder


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
        "mode": "host",
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


def test_sidecar_gateway_mounts_trusted_usage_log_outside_agent_workspace(
    monkeypatch, tmp_path: Path
) -> None:
    monkeypatch.setattr("harness.gateway_runtime.shutil.which", lambda _: "/usr/bin/docker")
    run_root = tmp_path / "run"
    workspace = run_root / "agent-workspace"
    workspace.mkdir(parents=True)
    runtime = DockerGatewaySidecarRuntime(
        config=GatewayConfig(mode="sidecar", sidecar_image="gateway:test"),
        run_dir=workspace,
        task_dir=tmp_path / "task",
        run_id="run_gateway_mount",
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "task"),
    )

    command = runtime._container_command()
    assert runtime.usage_event_path == run_root / "gateway.events.jsonl"
    assert (
        f"{runtime.usage_event_path.resolve()}:/gateway-events.jsonl:rw" in command
    )
    assert command[command.index("--trusted-event-file") + 1] == "/gateway-events.jsonl"


def test_container_agent_gateway_cannot_read_original_task_verifier(
    monkeypatch, tmp_path: Path
) -> None:
    import harness.orchestrator as orchestrator_module

    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    agent_path = tmp_path / "container-agent.yaml"
    agent_path.write_text("fixture", encoding="utf-8")
    agent = AgentSpec(api_version="harness/v1", id="container-probe", type="docker", image="probe")
    captured: dict[str, Path] = {}

    class GatewayProbe:
        mode = "host"
        network_name = None
        url = "http://127.0.0.1:8765"
        token = "gateway-token"

        def __init__(self, *, run_dir: Path, task_dir: Path, **_kwargs) -> None:
            self.run_dir = run_dir
            self.task_dir = task_dir
            self.usage_event_path = run_dir.parent / "gateway.events.jsonl"
            captured["task_dir"] = task_dir
            captured["workspace"] = run_dir

        def start(self) -> None:
            pass

        def stop(self) -> None:
            pass

    class FailingAgent:
        async def start_session(self, **_kwargs):
            raise RuntimeError("stop after Gateway path capture")

    monkeypatch.setattr(orchestrator_module, "build_gateway_runtime", lambda **kwargs: GatewayProbe(**kwargs))
    monkeypatch.setattr(orchestrator_module, "build_agent_adapter", lambda *_a, **_k: FailingAgent())

    result = Orchestrator(tmp_path / "runs").run(
        task=task,
        task_path=task_path,
        agent=agent,
        agent_path=agent_path,
        gateway_config=GatewayConfig(),
    )

    assert result["status"] == "error"
    assert captured["task_dir"] != task_path.resolve().parent
    assert not (captured["task_dir"] / "verifier.py").exists()
    app = create_gateway_app(
        event_file=captured["workspace"] / "gateway-test-events.jsonl",
        workspace=captured["workspace"],
        task_dir=captured["task_dir"],
        gateway_token="gateway-token",
    )
    response = TestClient(app).post(
        "/v1/tools/call",
        headers={"Authorization": "Bearer gateway-token"},
        json={
            "name": "file.read",
            "args": {"path": str(task_path.resolve().parent / "verifier.py")},
        },
    )
    assert response.status_code == 403
