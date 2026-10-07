from __future__ import annotations

import asyncio
import io
import json
from pathlib import Path
from types import SimpleNamespace

from harness.models import AgentSpec, TaskSpec
from harness.pi_container import ContainerPiAdapter
from harness.pi_rpc import ContainerPiRpcSession
from harness.session import AgentObservation
from harness.trace import TraceRecorder


class _FakeWriter:
    def __init__(self) -> None:
        self.buffer = io.BytesIO()

    def write(self, data: bytes) -> None:
        self.buffer.write(data)

    async def drain(self) -> None:
        return None

    def close(self) -> None:
        return None

    async def wait_closed(self) -> None:
        return None


class _FakeProcess:
    def __init__(self) -> None:
        self.stdin = _FakeWriter()
        self.stdout = None
        self.returncode = None


def _spec() -> AgentSpec:
    return AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "pi-rpc-test",
            "type": "pi",
            "image": "harness-pi-kali:test",
            "pi": {
                "provider": "fake",
                "model": "fake-model",
                "offline": False,
            },
        }
    )


def _task() -> TaskSpec:
    return TaskSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "task-rpc",
            "name": "RPC task",
            "objective": {"description": "solve rpc test"},
        }
    )


def test_pi_rpc_command_has_no_startup_prompt(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("harness.pi_container.shutil.which", lambda _: "/usr/bin/docker")
    adapter = ContainerPiAdapter(
        _spec(),
        allow_host=False,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-rpc", "task-rpc"),
    )
    command = adapter._rpc_command(_task(), gateway_enabled=False)

    assert command[command.index("--mode") + 1] == "rpc"
    assert "--no-session" in command
    assert "solve rpc test" not in command


def test_pi_rpc_container_keeps_stdin_open(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("harness.pi_container.shutil.which", lambda _: "/usr/bin/docker")
    adapter = ContainerPiAdapter(
        _spec(),
        allow_host=False,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-rpc", "task-rpc"),
    )
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    command = adapter._container_command(
        _task(),
        task_dir=tmp_path,
        run_dir=run_dir,
        container_name="pi-rpc",
        network="bridge",
        environment_project=None,
        seed=1,
        gateway_url=None,
        gateway_token=None,
        host_gateway=False,
        rpc=True,
    )

    assert "-i" in command
    assert command[command.index("--mode") + 1] == "rpc"


def test_pi_rpc_observe_steers_active_session(tmp_path: Path) -> None:
    task = _task()
    process = _FakeProcess()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-rpc", "task-rpc")
    adapter = SimpleNamespace(trace=trace, spec=_spec())
    stderr = io.BytesIO()
    session = ContainerPiRpcSession(
        adapter,
        run_kwargs={"task": task},
        run_dir=tmp_path,
        process=process,
        container_name="pi-rpc",
        stderr_handle=stderr,
        gateway_enabled=False,
    )

    asyncio.run(
        session.observe(
            AgentObservation(
                type="solver.verification",
                data={
                    "status": "contradicted",
                    "action_id": "action-1",
                    "replan_required": True,
                },
            )
        )
    )

    line = process.stdin.buffer.getvalue().decode("utf-8").strip()
    command = json.loads(line)
    assert command["type"] == "prompt"
    assert command["streamingBehavior"] == "steer"
    assert "solver.verification" in command["message"]
    assert '"action_id": "action-1"' in command["message"]
