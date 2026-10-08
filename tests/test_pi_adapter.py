import asyncio
import json
from pathlib import Path

from harness.models import AgentSpec, load_task
from harness.pi_container import ContainerPiAdapter, ContainerPiRpcSession
from harness.session import AgentObservation
from harness.trace import TraceRecorder


def _pi_spec(mode: str = "rpc") -> AgentSpec:
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
                "mode": mode,
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


def test_pi_command_uses_rpc_mode_and_safe_defaults(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    command = adapter._command(task, gateway_enabled=False)
    assert "--mode" in command
    assert command[command.index("--mode") + 1] == "rpc"
    assert "--no-session" in command
    assert "--no-approve" in command
    assert "--no-context-files" in command
    assert "--no-extensions" in command
    assert "--no-skills" in command
    assert "--no-mcp" in command
    assert command[command.index("--provider") + 1] == "fake"
    assert command[command.index("--model") + 1] == "fake-model"
    prompt = adapter._prompt_text(task)
    assert "$HARNESS_SUBMISSION_INBOX" in prompt
    assert "$HARNESS_FEEDBACK_FILE" in prompt
    assert "rejected candidate" in prompt
    assert "reread $HARNESS_WORLD_CONTEXT" in prompt


def test_pi_json_mode_remains_available_for_compatibility(monkeypatch, tmp_path: Path) -> None:
    spec = _pi_spec(mode="json")
    monkeypatch.setattr("harness.pi_container.shutil.which", lambda _: "/usr/bin/docker")
    adapter = ContainerPiAdapter(
        spec,
        allow_host=False,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))

    command = adapter._command(task, gateway_enabled=False)

    assert command[command.index("--mode") + 1] == "json"
    assert command[-1].startswith(task.objective.description)


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
        json.loads(line)["type"] for line in event_file.read_text(encoding="utf-8").splitlines()
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


def test_pi_rpc_session_sends_prompt_and_steers_before_settled(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "test-key")
    adapter = _adapter(monkeypatch, tmp_path)
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-rpc"
    run_dir.mkdir()
    writes: list[bytes] = []

    class FakeWriter:
        closed = False

        def write(self, value: bytes) -> None:
            writes.append(value)

        async def drain(self) -> None:
            return None

        def is_closing(self) -> bool:
            return self.closed

        def close(self) -> None:
            self.closed = True

    class FakeReader:
        def __init__(self) -> None:
            self.lines = [
                {"type": "session", "id": "session-1", "version": 1},
                {
                    "type": "message_start",
                    "message": {"role": "assistant", "provider": "fake", "model": "fake-model"},
                },
                {"type": "tool_execution_start", "toolCallId": "call-1", "toolName": "bash"},
                {"type": "tool_execution_end", "toolCallId": "call-1", "toolName": "bash"},
                {
                    "type": "message_end",
                    "message": {
                        "role": "assistant",
                        "provider": "fake",
                        "model": "fake-model",
                        "usage": {"input": 3, "output": 2, "totalTokens": 5},
                    },
                },
                {"type": "agent_end"},
                {"type": "agent_settled"},
                {
                    "type": "message_start",
                    "message": {"role": "assistant", "provider": "fake", "model": "fake-model"},
                },
                {
                    "type": "message_end",
                    "message": {
                        "role": "assistant",
                        "provider": "fake",
                        "model": "fake-model",
                        "usage": {"input": 4, "output": 1, "totalTokens": 5},
                    },
                },
                {"type": "agent_end"},
                {"type": "agent_settled"},
            ]

        async def readline(self) -> bytes:
            if not self.lines:
                return b""
            return (json.dumps(self.lines.pop(0)) + "\n").encode()

    class FakeProcess:
        def __init__(self) -> None:
            self.stdin = FakeWriter()
            self.stdout = FakeReader()
            self.returncode = None

        async def wait(self) -> int:
            self.returncode = 0
            return 0

    fake_process = FakeProcess()
    monkeypatch.setattr(
        adapter,
        "_docker",
        lambda command, *, cwd, check=True: type(
            "Completed", (), {"returncode": 0, "stdout": "", "stderr": ""}
        )(),
    )
    monkeypatch.setattr(
        "harness.pi_container.asyncio.create_subprocess_exec",
        lambda *args, **kwargs: asyncio.sleep(0, result=fake_process),
    )
    monkeypatch.setattr(
        "harness.pi_container.subprocess.run",
        lambda *args, **kwargs: type("Completed", (), {"returncode": 0})(),
    )

    async def scenario():
        session = await ContainerPiRpcSession.start(
            adapter,
            run_kwargs={
                "task": task,
                "task_dir": Path("benchmarks/examples/hello").resolve(),
                "run_dir": run_dir,
                "environment_project": None,
                "environment_network": None,
                "seed": 1,
            },
        )
        await session.observe(
            AgentObservation(type="solver.plan.updated", data={"actions": ["probe"]})
        )
        events = []
        settled_count = 0
        async for event in session.events():
            events.append(event)
            if event.type == "tool.call":
                await session.observe(
                    AgentObservation(
                        type="solver.verification",
                        data={"status": "pending", "replan_required": True},
                    )
                )
            elif event.type == "pi.agent_settled":
                settled_count += 1
                if settled_count == 1:
                    await session.observe(
                        AgentObservation(
                            type="benchmark.feedback",
                            data={"accepted": False, "completed": False},
                        )
                    )
        result = await session.result()
        return events, result

    events, result = asyncio.run(scenario())
    commands = [json.loads(line) for line in b"".join(writes).decode().splitlines()]

    assert commands[0]["type"] == "set_steering_mode"
    assert commands[0]["mode"] == "all"
    assert commands[1]["type"] == "prompt"
    assert "solver.plan.updated" in commands[1]["message"]
    assert commands[2]["type"] == "steer"
    assert "solver.verification" in commands[2]["message"]
    assert commands[3]["type"] == "prompt"
    assert "benchmark.feedback" in commands[3]["message"]
    assert [event.type for event in events] == [
        "pi.session",
        "model.request",
        "tool.call",
        "tool.result",
        "model.response",
        "model.usage",
        "pi.agent_settled",
        "model.request",
        "model.response",
        "model.usage",
        "pi.agent_settled",
    ]
    assert len({event.event_id for event in events}) == len(events)
    assert result.metrics.tool_calls == 1
    assert result.metrics.total_tokens == 10


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
