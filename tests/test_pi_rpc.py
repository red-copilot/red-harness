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
    assert set(event.type for event in events).issubset(AGENT_CLAIMS)
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
