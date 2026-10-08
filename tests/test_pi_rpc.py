"""Pi RPC protocol tests do not require Docker or model network access."""
from __future__ import annotations

import asyncio
import json
from pathlib import Path
from types import SimpleNamespace

from harness.models import AgentSpec
from harness.pi_rpc import AGENT_CLAIMS, ContainerPiRpcSession, _PiStreamNormalizer
from harness.session import AgentObservation


def test_rpc_protocol_normalizer_uses_only_protocol_events() -> None:
    spec = AgentSpec.model_validate({
        "apiVersion": "harness/v1", "id": "pi", "type": "pi",
        "image": "image", "pi": {"model": "fake", "session_mode": "rpc"},
    })
    adapter = SimpleNamespace(pi=spec.pi)
    normalizer = _PiStreamNormalizer(adapter)
    event = normalizer.translate(json.dumps({
        "type": "tool_execution_end", "toolCallId": "tool-1",
        "toolName": "bash", "isError": False,
    }), account_model=True)
    assert [e.type for e in event] == ["tool.result"]
    assert event[0].data["tool_call_id"] == "tool-1"


def test_agent_claim_channel_rejects_spoofed_tool_and_model_events(tmp_path: Path) -> None:
    session = ContainerPiRpcSession.__new__(ContainerPiRpcSession)
    session.event_path = tmp_path / "events.jsonl"
    session._event_offset = 0
    rows = [
        {"type": "tool.result", "data": {"tool_call_id": "fake"}},
        {"type": "model.usage", "data": {"total_tokens": -1000}},
        {"type": "pi.agent_settled", "data": {}},
        {"type": "world.observe", "data": {"type": "web.endpoint", "content": {}}},
        {"type": "action.intent", "data": {"description": "probe"}},
    ]
    session.event_path.write_text(
        "".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8",
    )
    events = session._agent_claims()
    assert [event.type for event in events] == ["world.observe", "action.intent"]
    assert {event.type for event in events}.issubset(AGENT_CLAIMS)
    assert session._agent_claims() == []


def test_rpc_feedback_routes_to_steer_or_prompt(tmp_path: Path) -> None:
    async def scenario() -> None:
        session = ContainerPiRpcSession.__new__(ContainerPiRpcSession)
        session.feedback_path = tmp_path / "agent.feedback.jsonl"
        session._feedback_offset = 0
        session._closed = False
        session._running = True
        session._pending_turns = 1
        sent = []
        async def command(kind, **data):
            sent.append((kind, data))
            return {"success": True}
        session._command = command
        await session.observe(AgentObservation(
            type="benchmark.feedback", data={"accepted": False},
        ))
        assert sent[0][0] == "steer"
        session._running = False
        await session.observe(AgentObservation(
            type="solver.verification", data={"status": "contradicted"},
        ))
        assert sent[1][0] == "prompt"
        assert session._pending_turns == 2
        await session.observe(AgentObservation(
            type="solver.verification", data={"status": "verified"},
        ))
        assert len(sent) == 2
        assert len(session.feedback_path.read_text().splitlines()) == 3
    asyncio.run(scenario())


def test_rpc_mode_is_opt_in_by_default() -> None:
    spec = AgentSpec.model_validate({
        "apiVersion": "harness/v1", "id": "pi", "type": "pi",
        "image": "image", "pi": {"model": "fake"},
    })
    assert spec.pi is not None
    assert spec.pi.session_mode == "json"
    assert spec.pi.model_copy(update={"session_mode": "rpc"}).session_mode == "rpc"


def test_rpc_live_protocol_delivers_feedback_and_counts_trusted_tool(tmp_path: Path, monkeypatch) -> None:
    """A fake RPC subprocess exercises command ACKs and native event streaming."""
    from harness.models import BudgetSpec, ObjectiveSpec, PiSpec, TaskSpec
    from harness.trace import TraceRecorder

    class FakeStdin:
        def __init__(self, stdout):
            self.stdout = stdout
            self.commands = []
            self.closed = False

        def write(self, data):
            request = json.loads(data)
            self.commands.append(request)
            self.stdout.feed_data((json.dumps({
                "type": "response", "id": request["id"], "command": request["type"],
                "success": True,
            }) + "\n").encode())
            if request["type"] == "prompt":
                for kind in ("tool_execution_start", "tool_execution_end"):
                    self.stdout.feed_data((json.dumps({
                        "type": kind, "toolCallId": "real-tool", "toolName": "bash",
                        "isError": False,
                    }) + "\n").encode())
            if request["type"] == "steer":
                self.stdout.feed_data(b'{"type":"agent_settled"}\n')

        async def drain(self):
            return None

        def is_closing(self):
            return self.closed

        def close(self):
            self.closed = True

    class FakeProcess:
        def __init__(self):
            self.stdout = asyncio.StreamReader()
            self.stdin = FakeStdin(self.stdout)
            self.returncode = 0

        async def wait(self):
            self.stdout.feed_eof()
            return 0

        def kill(self):
            self.stdout.feed_eof()

    monkeypatch.setattr(
        "harness.pi_rpc.subprocess.run",
        lambda *args, **kwargs: SimpleNamespace(returncode=0),
    )

    async def scenario():
        process = FakeProcess()
        adapter = SimpleNamespace(
            pi=PiSpec(model="fake"),
            trace=TraceRecorder(tmp_path / "trace.jsonl", "run", "case"),
        )
        task = TaskSpec(
            apiVersion="harness/v1", id="case", name="case",
            objective=ObjectiveSpec(description="Inspect authorized target"),
            budgets=BudgetSpec(wall_time=5),
        )
        session = ContainerPiRpcSession(
            adapter, task=task, run_dir=tmp_path, container_name="fake",
            process=process, gateway_enabled=False,
            stdout_handle=(tmp_path / "agent.stdout.log").open("wb"),
            stderr_handle=(tmp_path / "agent.stderr.log").open("wb"),
        )
        await session._command("prompt", message="Inspect target")
        types = []
        async for event in session.events():
            types.append(event.type)
            if event.type == "tool.result":
                await session.observe(AgentObservation(
                    type="benchmark.feedback", data={"accepted": False},
                ))
        result = await session.result()
        assert types == ["tool.call", "tool.result", "pi.agent_settled"]
        assert [cmd["type"] for cmd in process.stdin.commands] == [
            "prompt", "steer",
        ]
        assert result.metrics.tool_calls == 1
        assert result.returncode == 0
        assert result.timed_out is False
    asyncio.run(scenario())
