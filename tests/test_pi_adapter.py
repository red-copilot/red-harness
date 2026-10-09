import asyncio
import hashlib
import json
import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest

from harness.agent import AgentError
from harness.budget import UsageMetrics
from harness.models import AgentSpec, load_task
from harness.pi_container import ContainerPiAdapter, ContainerPiRpcSession
from harness.session import AgentEvent, AgentObservation
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


def test_resume_refuses_to_replace_running_pi_container(tmp_path: Path) -> None:
    calls = []

    def docker(command, *, cwd, check=True):
        calls.append(command)
        return subprocess.CompletedProcess(command, 0, stdout="true\n", stderr="")

    session = object.__new__(ContainerPiRpcSession)
    session.adapter = SimpleNamespace(_docker=docker)
    session.container_name = "harness_pi_run"

    with pytest.raises(AgentError, match="still running"):
        asyncio.run(session._guard_existing_resume_container(tmp_path))

    assert len(calls) == 1
    assert calls[0][:3] == ["docker", "inspect", "--format"]


def test_resume_removes_only_a_stopped_pi_container(tmp_path: Path) -> None:
    calls = []

    def docker(command, *, cwd, check=True):
        calls.append(command)
        if command[1] == "inspect":
            return subprocess.CompletedProcess(command, 0, stdout="false\n", stderr="")
        return subprocess.CompletedProcess(command, 0, stdout="", stderr="")

    session = object.__new__(ContainerPiRpcSession)
    session.adapter = SimpleNamespace(_docker=docker)
    session.container_name = "harness_pi_run"

    asyncio.run(session._guard_existing_resume_container(tmp_path))

    assert [call[1] for call in calls] == ["inspect", "rm"]


def test_pi_rpc_session_rejects_unsafe_or_unexpected_startup_session_id() -> None:
    class _Reader:
        def __init__(self, session_id):
            self.session_id = session_id

        async def readline(self):
            return (json.dumps({"type": "session", "id": self.session_id}) + "\n").encode()

    async def verify(session_id, requested_id=None):
        session = object.__new__(ContainerPiRpcSession)
        session.process = SimpleNamespace(stdout=_Reader(session_id))
        session.run_kwargs = {"resume_session_id": requested_id}
        session._startup_records = []
        session._pi_session_id = None
        await session._wait_for_session_identity()

    with pytest.raises(AgentError, match="invalid session ID"):
        asyncio.run(verify("../unsafe"))
    with pytest.raises(AgentError, match="did not restore"):
        asyncio.run(verify("new-session", "expected-session"))


def test_pi_command_uses_rpc_mode_and_safe_defaults(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    command = adapter._command(task, gateway_enabled=False)
    assert "--mode" in command
    assert command[command.index("--mode") + 1] == "rpc"
    assert "--no-session" not in command
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


def test_pi_rpc_command_can_select_a_checkpointed_session(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))

    command = adapter._command(task, gateway_enabled=False, session_id="pi-session-123")

    assert command[command.index("--session") + 1] == "pi-session-123"
    try:
        adapter._command(task, gateway_enabled=False, session_id="../../etc/passwd")
    except AgentError as exc:
        assert "invalid Pi session ID" in str(exc)
    else:
        raise AssertionError("unsafe session references must be rejected")


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
    workspace = run_dir / "agent-workspace"
    workspace.mkdir()

    command = adapter._container_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=workspace,
        container_name="pi-test",
        network="bridge",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )
    assert "--read-only" in command
    assert command[command.index("--user") + 1] == (
        f"{workspace.stat().st_uid}:{workspace.stat().st_gid}"
    )
    assert command[command.index("--cap-drop") + 1] == "ALL"
    assert "--cap-add" not in command
    assert "no-new-privileges" in command
    assert command[command.index("--pids-limit") + 1] == "512"
    assert command[command.index("--memory") + 1] == "4g"
    assert command[command.index("--cpus") + 1] == "4"
    assert "/tmp:rw,nosuid,nodev,size=512m" in command
    assert "FAKE_PROVIDER_KEY=secret" in command
    assert "HARNESS_EVENT_FILE=/run/harness/events.jsonl" in command
    assert f"{workspace.resolve()}:/run/harness:rw" in command
    assert f"{run_dir.resolve()}:/run/harness:rw" not in command
    task_mount = next(value for value in command if value.endswith(":/task:ro"))
    assert task_mount != f"{Path('benchmarks/examples/hello').resolve()}:/task:ro"
    assert not (run_dir / "agent-task-input" / "verifier.py").exists()
    assert "HARNESS_NETWORK_PROFILE=unrestricted" in command
    assert "harness-pi-kali:test" in command


def test_strict_pi_profile_clears_docker_proxy_and_rejects_proxy_passthrough(
    monkeypatch, tmp_path: Path
) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    adapter.spec.network_profile = "model-allowed"
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-strict-proxy"
    run_dir.mkdir()

    command = adapter._container_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        container_name="pi-strict-proxy",
        network="gateway-internal",
        environment_project=None,
        seed=1,
        gateway_url="http://harness-gateway:8765",
        gateway_token="gateway-token",
        host_gateway=False,
    )
    assert "HTTP_PROXY=" in command
    assert "https_proxy=" in command

    adapter.pi.env_passthrough.append("ALL_PROXY")
    with pytest.raises(AgentError, match="strict network profile.*proxy"):
        adapter._container_command(
            task,
            task_dir=Path("benchmarks/examples/hello").resolve(),
            run_dir=run_dir,
            container_name="pi-strict-proxy",
            network="gateway-internal",
            environment_project=None,
            seed=1,
            gateway_url="http://harness-gateway:8765",
            gateway_token="gateway-token",
            host_gateway=False,
        )


def test_pi_only_restores_explicitly_configured_capabilities(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "secret")
    adapter.pi.cap_add = ["NET_RAW"]
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-capability"
    run_dir.mkdir()

    command = adapter._container_command(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        container_name="pi-explicit-capability",
        network="bridge",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
    )

    assert command[command.index("--cap-add") + 1] == "NET_RAW"


def test_pi_launch_rechecks_reserved_environment_names(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    adapter.pi.env_passthrough = ["BENCHMARK_TOKEN"]
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-reserved-env"
    run_dir.mkdir()

    with pytest.raises(AgentError, match="reserved credential environment"):
        adapter._container_command(
            task,
            task_dir=Path("benchmarks/examples/hello").resolve(),
            run_dir=run_dir,
            container_name="pi-reserved-env",
            network="none",
            environment_project=None,
            seed=1,
            gateway_url=None,
            gateway_token=None,
            host_gateway=False,
        )


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


def test_pi_usage_with_malformed_counters_is_preserved_for_fail_closed_handling(
    monkeypatch, tmp_path: Path
) -> None:
    adapter = _adapter(monkeypatch, tmp_path)

    normalized = adapter._normalize_line(
        json.dumps(
            {
                "type": "message_end",
                "message": {
                    "role": "assistant",
                    "usage": {
                        "input": "invalid",
                        "output": 2,
                        "totalTokens": -10,
                        "cost": {"total": 0.1},
                    },
                },
            }
        ),
        account_model=True,
    )

    assert normalized[-1][0] == "model.usage"
    assert normalized[-1][1]["input_tokens"] == "invalid"
    assert normalized[-1][1]["total_tokens"] == -10

    session = object.__new__(ContainerPiRpcSession)
    session.adapter = adapter
    session._metrics = UsageMetrics()
    session._budget_exceeded = None
    session._account(AgentEvent(type="model.usage", data=normalized[-1][1]))

    assert session._budget_exceeded == "invalid_telemetry"
    assert session._metrics.total_tokens == 0


def test_gateway_mode_accounts_model_usage_from_gateway_only(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    session = ContainerPiRpcSession(
        adapter,
        run_kwargs={
            "run_dir": tmp_path,
            "task": load_task(Path("benchmarks/examples/hello/task.yaml")),
            "gateway_url": "http://gateway:8765",
            "gateway_token": "test-token",
            "gateway_usage_path": tmp_path / "gateway.events.jsonl",
        },
    )

    pi_events = session._normalize_pi_record(
        json.dumps(
            {
                "type": "message_end",
                "message": {
                    "role": "assistant",
                    "usage": {"input": 3, "output": 2, "totalTokens": 5},
                },
            }
        )
    )
    session._consume_gateway_event_line(
        json.dumps({"type": "model.request", "data": {}}).encode() + b"\n"
    )
    session._consume_gateway_event_line(
        json.dumps(
            {
                "type": "model.usage",
                "data": {
                    "input_tokens": 3,
                    "output_tokens": 2,
                    "total_tokens": 5,
                    "cost_usd": 0.01,
                },
            }
        ).encode()
        + b"\n"
    )

    assert pi_events == []
    assert session._metrics.model_calls == 1
    assert session._metrics.total_tokens == 5
    assert session._metrics.cost_usd == 0.01


def test_pi_message_end_without_usage_emits_missing_usage_event(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)

    normalized = adapter._normalize_line(
        json.dumps(
            {
                "type": "message_end",
                "message": {"role": "assistant", "provider": "fixture"},
            }
        ),
        account_model=True,
    )

    assert [event_type for event_type, _data in normalized] == [
        "model.response",
        "model.usage_missing",
    ]


def test_pi_rpc_rejects_missing_usage_event(monkeypatch, tmp_path: Path) -> None:
    from harness.budget import UsageMetrics

    adapter = _adapter(monkeypatch, tmp_path)
    session = object.__new__(ContainerPiRpcSession)
    session.adapter = adapter
    session._metrics = UsageMetrics()
    session._budget_exceeded = None

    session._account(AgentEvent(type="model.usage_missing", data={"source": "pi"}))

    assert session._budget_exceeded == "invalid_telemetry"
    assert session._metrics.total_tokens == 0


def test_pi_usage_rejects_total_below_input_and_output_sum(monkeypatch, tmp_path: Path) -> None:
    adapter = _adapter(monkeypatch, tmp_path)
    session = object.__new__(ContainerPiRpcSession)
    session.adapter = adapter
    session._metrics = UsageMetrics()
    session._budget_exceeded = None

    session._account(
        AgentEvent(
            type="model.usage",
            data={"input_tokens": 6, "output_tokens": 5, "total_tokens": 1},
        )
    )

    assert session._budget_exceeded == "invalid_telemetry"
    assert session._metrics.total_tokens == 0


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
                b"not-json rpc record\n",
                {
                    "type": "response",
                    "id": "command-1",
                    "success": False,
                    "error": "api_key=provider-secret",
                },
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
            line = self.lines.pop(0)
            return line if isinstance(line, bytes) else (json.dumps(line) + "\n").encode()

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
        initial_checkpoint = await session.checkpoint()
        assert initial_checkpoint.session_id == "session-1"
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
        "agent.telemetry_error",
        "agent.telemetry_error",
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
    rpc_error = events[2]
    assert rpc_error.data == {
        "source": "pi.rpc",
        "command_id_sha256": hashlib.sha256(b"command-1").hexdigest(),
        "error_type": "str",
        "message": "Pi RPC command failed",
    }
    assert "provider-secret" not in (run_dir / "events.jsonl").read_text(encoding="utf-8")
    assert "provider-secret" not in (tmp_path / "trace.jsonl").read_text(encoding="utf-8")


@pytest.mark.parametrize(
    ("budget_name", "limit", "records", "event_types", "metric_name", "metric_value"),
    [
        (
            "max_tokens",
            2,
            [
                {
                    "type": "message_end",
                    "message": {
                        "role": "assistant",
                        "usage": {"input": 2, "output": 2, "totalTokens": 4},
                    },
                }
            ],
            ["pi.session", "model.response", "model.usage"],
            "total_tokens",
            4,
        ),
        (
            "max_model_calls",
            1,
            [
                {"type": "message_start", "message": {"role": "assistant"}},
                {"type": "message_start", "message": {"role": "assistant"}},
            ],
            ["pi.session", "model.request", "model.request"],
            "model_calls",
            2,
        ),
        (
            "max_tool_calls",
            1,
            [
                {"type": "tool_execution_start", "toolCallId": "tool-1", "toolName": "bash"},
                {"type": "tool_execution_start", "toolCallId": "tool-2", "toolName": "bash"},
            ],
            ["pi.session", "tool.call", "tool.call"],
            "tool_calls",
            2,
        ),
        (
            "max_cost_usd",
            0.005,
            [
                {
                    "type": "message_end",
                    "message": {
                        "role": "assistant",
                        "usage": {
                            "input": 2,
                            "output": 2,
                            "totalTokens": 4,
                            "cost": {"total": 0.01},
                        },
                    },
                }
            ],
            ["pi.session", "model.response", "model.usage"],
            "cost_usd",
            0.01,
        ),
    ],
)
def test_pi_rpc_session_kills_container_when_budget_is_exceeded(
    monkeypatch,
    tmp_path: Path,
    budget_name: str,
    limit: float,
    records: list[dict],
    event_types: list[str],
    metric_name: str,
    metric_value: float,
) -> None:
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "test-key")
    adapter = _adapter(monkeypatch, tmp_path)
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    task = task.model_copy(
        update={"budgets": task.budgets.model_copy(update={budget_name: limit})}
    )
    run_dir = tmp_path / "run-budget-rpc"
    run_dir.mkdir()
    killed: list[str] = []

    class FakeWriter:
        def write(self, value: bytes) -> None:
            pass

        async def drain(self) -> None:
            pass

        def is_closing(self) -> bool:
            return False

        def close(self) -> None:
            pass

    class FakeReader:
        def __init__(self) -> None:
            self.lines = [{"type": "session", "id": "budget-session"}, *records]
            self.lines.append({"type": "agent_settled"})

        async def readline(self) -> bytes:
            if not self.lines:
                return b""
            return (json.dumps(self.lines.pop(0)) + "\n").encode()

    class FakeProcess:
        stdin = FakeWriter()
        stdout = FakeReader()
        returncode = None

        async def wait(self) -> int:
            self.returncode = -9
            return self.returncode

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

    def fake_run(command, **kwargs):
        if command[1] == "kill":
            killed.append(command[-1])
        return type("Completed", (), {"returncode": 0})()

    monkeypatch.setattr("harness.pi_container.subprocess.run", fake_run)

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
        events = [event async for event in session.events()]
        result = await session.result()
        return events, result

    events, result = asyncio.run(scenario())

    assert [event.type for event in events] == event_types
    assert killed
    assert set(killed) == {"harness_pi_run_budget_rpc"}
    assert result.budget_exceeded == budget_name
    assert getattr(result.metrics, metric_name) == metric_value


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


def test_pi_rpc_uses_verified_target_network_id(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("FAKE_PROVIDER_KEY", "test-provider-key")
    adapter = _adapter(monkeypatch, tmp_path)
    adapter.spec.network_profile = "target-only"
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-strict-rpc"
    run_dir.mkdir()
    agent_task_dir = tmp_path / "agent-task"
    agent_task_dir.mkdir()
    docker_commands = []

    class Reader:
        async def readline(self):
            return b'{"type":"session","id":"session-1"}\n'

    class Process:
        stdout = Reader()
        stdin = object()
        returncode = None

    def inspect(command, **kwargs):
        return subprocess.CompletedProcess(
            command, 0, stdout=f"{'b' * 64} true\n", stderr=""
        )

    monkeypatch.setattr("harness.network_policy.subprocess.run", inspect)
    monkeypatch.setattr(adapter, "_prepare_agent_dir", lambda **kwargs: None)
    monkeypatch.setattr(
        adapter,
        "_docker",
        lambda command, **kwargs: docker_commands.append(command)
        or subprocess.CompletedProcess(command, 0, stdout="", stderr=""),
    )
    monkeypatch.setattr(
        "harness.pi_container.asyncio.create_subprocess_exec",
        lambda *args, **kwargs: asyncio.sleep(0, result=Process()),
    )

    async def start_session():
        session = await ContainerPiRpcSession.start(
            adapter,
            run_kwargs={
                "task": task,
                "task_dir": Path("benchmarks/examples/hello").resolve(),
                "agent_task_dir": agent_task_dir,
                "run_dir": run_dir,
                "environment_project": None,
                "environment_network": "task-internal",
                "seed": 1,
            },
        )
        session._stderr_handle.close()

    asyncio.run(start_session())

    create = docker_commands[0]
    assert create[create.index("--network") + 1] == "b" * 64
    assert len(docker_commands) == 1


def test_legacy_pi_json_runner_uses_verified_target_network_id(monkeypatch, tmp_path: Path) -> None:
    from harness.pi_adapter import PiAdapter

    adapter_spec = _pi_spec(mode="json").model_copy(
        update={"network_profile": "target-only"}
    )
    monkeypatch.setattr("harness.pi_adapter.shutil.which", lambda _: "/usr/bin/docker")
    adapter = PiAdapter(
        adapter_spec,
        allow_host=False,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    run_dir = tmp_path / "run-strict-json"
    run_dir.mkdir()
    commands = []

    def fake_subprocess_run(command, **kwargs):
        commands.append(command)
        stdout = f"{'c' * 64} true\n" if command[:3] == ["docker", "network", "inspect"] else ""
        return subprocess.CompletedProcess(command, 0, stdout=stdout, stderr="")

    class Process:
        returncode = 0

        def poll(self):
            return self.returncode

        def wait(self):
            return self.returncode

    monkeypatch.setattr("harness.network_policy.subprocess.run", fake_subprocess_run)
    monkeypatch.setattr("harness.pi_adapter.subprocess.Popen", lambda *args, **kwargs: Process())

    adapter.run(
        task,
        task_dir=Path("benchmarks/examples/hello").resolve(),
        run_dir=run_dir,
        environment_project=None,
        environment_network="task-internal",
        seed=1,
    )

    create = next(command for command in commands if command[:2] == ["docker", "create"])
    assert create[create.index("--network") + 1] == "c" * 64
