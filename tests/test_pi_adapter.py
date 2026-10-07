import json
from pathlib import Path

from harness.models import AgentSpec, load_task
from harness.pi_container import ContainerPiAdapter
from harness.trace import TraceRecorder


def _pi_spec() -> AgentSpec:
    return AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "pi-container-test",
            "type": "pi",
            "image": "harness-pi-kali:test",
            "network": "environment",
            "pi": {
                "provider": "fake",
                "model": "fake-model",
                "thinking": "medium",
                "env_passthrough": ["FAKE_PROVIDER_KEY"],
            },
        }
    )


def _adapter(monkeypatch, tmp_path: Path) -> ContainerPiAdapter:
    monkeypatch.setattr("harness.pi_container.shutil.which", lambda _: "/usr/bin/docker")
    return ContainerPiAdapter(
        _pi_spec(),
        allow_host=False,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )


def test_pi_command_uses_safe_noninteractive_defaults(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    command = adapter._command(task, gateway_enabled=False)
    assert "--mode" in command
    assert "json" in command
    assert "--no-session" in command
    assert "--no-approve" in command
    assert "--no-context-files" in command
    assert "--no-extensions" in command
    assert "--no-skills" in command
    assert "--no-mcp" in command
    assert command[command.index("--provider") + 1] == "fake"
    assert command[command.index("--model") + 1] == "fake-model"


def test_pi_gateway_writes_container_visible_provider(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    agent_dir = adapter._prepare_agent_dir(
        run_dir=tmp_path,
        gateway_url="http://harness-gateway:8765",
        gateway_token="one-time-token",
    )
    models = json.loads((agent_dir / "models.json").read_text(encoding="utf-8"))
    provider = models["providers"]["harness"]
    assert provider["baseUrl"] == "http://harness-gateway:8765/v1"
    assert provider["api"] == "openai-completions"
    assert provider["apiKey"] == "$HARNESS_GATEWAY_TOKEN"


def test_pi_container_command_is_restricted_and_passes_direct_secret(
    monkeypatch, tmp_path: Path
) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "secret")
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run"
    run_dir.mkdir()

    command = adapter._container_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        container_name="pi-test",
        network="bridge",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )
    assert "--read-only" in command
    assert command[command.index("--cap-drop") + 1] == "ALL"
    assert "NET_RAW" in command
    assert "FAKE_PROVIDER_KEY=secret" in command
    assert "HARNESS_EVENT_FILE=/run/harness/events.jsonl" in command
    assert "HARNESS_NETWORK_PROFILE=unrestricted" in command
    assert "harness-pi-kali:test" in command


def test_pi_gateway_does_not_pass_direct_provider_secret(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "secret")
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run"
    run_dir.mkdir()

    command = adapter._container_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        container_name="pi-test",
        network="gateway-net",
        environment_project=None,
        seed=1,
        gateway_url="http://harness-gateway:8765",
        gateway_token="one-time-token",
        host_gateway=False,
    )
    assert "FAKE_PROVIDER_KEY=secret" not in command
    assert "HARNESS_GATEWAY_TOKEN=one-time-token" in command


def test_pi_json_protocol_normalizes_model_and_tool_events(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    event_file = tmp_path / "events.jsonl"
    event_file.write_text("", encoding="utf-8")
    records = [
        {"type": "session", "version": 3, "id": "s1", "cwd": "/run/harness"},
        {
            "type": "message_start",
            "message": {"role": "assistant", "provider": "fake", "model": "fake-model"},
        },
        {"type": "tool_execution_start", "toolCallId": "c1", "toolName": "bash"},
        {
            "type": "tool_execution_end",
            "toolCallId": "c1",
            "toolName": "bash",
            "isError": False,
        },
        {
            "type": "message_end",
            "message": {
                "role": "assistant",
                "provider": "fake",
                "model": "fake-model",
                "stopReason": "stop",
                "usage": {
                    "input": 10,
                    "output": 5,
                    "totalTokens": 15,
                    "cost": {"total": 0.01},
                },
            },
        },
        {"type": "agent_settled"},
    ]
    for record in records:
        adapter._translate_line(
            json.dumps(record),
            event_file=event_file,
            account_model=True,
        )

    types = [
        json.loads(line)["type"]
        for line in event_file.read_text(encoding="utf-8").splitlines()
    ]
    assert types == [
        "pi.session",
        "model.request",
        "tool.call",
        "tool.result",
        "model.response",
        "model.usage",
        "pi.agent_settled",
    ]



def test_pi_offline_network_profile_forces_none(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "secret")
    spec = _pi_spec().model_copy(update={"network_profile": "offline"})
    monkeypatch.setattr("harness.pi_container.shutil.which", lambda _: "/usr/bin/docker")
    adapter = ContainerPiAdapter(
        spec,
        allow_host=False,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-offline"
    run_dir.mkdir()

    command = adapter._container_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        container_name="pi-offline",
        network="none",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )
    assert command[command.index("--network") + 1] == "none"
    assert "HARNESS_NETWORK_PROFILE=offline" in command
