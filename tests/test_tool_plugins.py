import asyncio
import json
import shutil
from pathlib import Path

import httpx
import pytest
from fastapi.testclient import TestClient

from harness.gateway import create_gateway_app
from harness.policy import GatewayPolicy
from harness.tool_adapter import ToolExecutionContext
from harness.tool_plugins import AuthorizedHTTPToolAdapter, SandboxedProcessToolAdapter


def _context(workspace: Path, task_dir: Path) -> ToolExecutionContext:
    return ToolExecutionContext(
        call_id="test-call",
        workspace=workspace,
        task_dir=task_dir,
        cancelled=asyncio.Event(),
    )


@pytest.mark.skipif(
    shutil.which("bwrap") is None or shutil.which("prlimit") is None,
    reason="bubblewrap and prlimit are required",
)
def test_sandboxed_process_sees_only_workspace_and_task_inputs(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_dir = tmp_path / "task"
    workspace.mkdir()
    task_dir.mkdir()
    (task_dir / "input.txt").write_text("challenge input", encoding="utf-8")
    host_secret = tmp_path / "host-secret.txt"
    host_secret.write_text("must remain hidden", encoding="utf-8")
    adapter = SandboxedProcessToolAdapter()

    result = asyncio.run(
        adapter.execute(
            {
                "argv": [
                    "/bin/sh",
                    "-c",
                    (
                        "cat /task/input.txt; printf artifact > /workspace/output.txt; "
                        "test ! -e /etc/passwd && echo isolated-root"
                    ),
                ]
            },
            _context(workspace, task_dir),
        )
    )

    assert result.output["exit_code"] == 0
    assert result.output["stdout"] == "challenge inputisolated-root\n"
    assert (workspace / "output.txt").read_text(encoding="utf-8") == "artifact"
    assert "must remain hidden" not in result.output["stdout"]
    assert result.metadata == {"sandbox": "bubblewrap", "network": "none"}


@pytest.mark.skipif(
    shutil.which("bwrap") is None or shutil.which("prlimit") is None,
    reason="bubblewrap and prlimit are required",
)
def test_sandboxed_process_enforces_wall_time_and_output_limits(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_dir = tmp_path / "task"
    workspace.mkdir()
    task_dir.mkdir()
    timed = asyncio.run(
        SandboxedProcessToolAdapter(timeout_seconds=0.1).execute(
            {"argv": ["/bin/sleep", "5"]}, _context(workspace, task_dir)
        )
    )
    noisy = asyncio.run(
        SandboxedProcessToolAdapter(output_limit_bytes=128).execute(
            {"argv": ["/usr/bin/yes"]}, _context(workspace, task_dir)
        )
    )

    assert timed.output["timed_out"] is True
    assert len(noisy.output["stdout"].encode()) <= 128
    assert noisy.output["truncated"] is True


@pytest.mark.skipif(
    shutil.which("bwrap") is None or shutil.which("prlimit") is None,
    reason="bubblewrap and prlimit are required",
)
def test_sandboxed_process_stops_when_gateway_signals_cancellation(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_dir = tmp_path / "task"
    workspace.mkdir()
    task_dir.mkdir()

    async def scenario() -> None:
        context = _context(workspace, task_dir)
        operation = asyncio.create_task(
            SandboxedProcessToolAdapter(timeout_seconds=10).execute(
                {"argv": ["/bin/sleep", "30"]}, context
            )
        )
        await asyncio.sleep(0.1)
        context.cancelled.set()
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(operation, timeout=2)

    asyncio.run(scenario())


def test_process_tool_rejects_workspace_escape_and_malformed_argv(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_dir = tmp_path / "task"
    workspace.mkdir()
    task_dir.mkdir()
    adapter = SandboxedProcessToolAdapter()

    with pytest.raises(ValueError, match="cwd must remain"):
        asyncio.run(
            adapter.execute({"argv": ["true"], "cwd": "../"}, _context(workspace, task_dir))
        )
    with pytest.raises(ValueError, match="argv must be an array"):
        asyncio.run(adapter.execute({"argv": "id"}, _context(workspace, task_dir)))


def test_authorized_http_tool_bounds_response_and_does_not_follow_redirects() -> None:
    calls: list[httpx.Request] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(302, headers={"location": "http://192.0.2.9/"}, text="abcdef")

    adapter = AuthorizedHTTPToolAdapter(
        allowed_cidrs=["127.0.0.0/8"],
        allowed_ports=[8080],
        max_response_bytes=4,
        transport=httpx.MockTransport(handler),
    )
    workspace = Path("/tmp")
    result = asyncio.run(
        adapter.execute(
            {"url": "http://127.0.0.1:8080/resource", "method": "GET"},
            _context(workspace, workspace),
        )
    )

    assert len(calls) == 1
    assert result.output == {
        "status_code": 302,
        "content_type": "text/plain; charset=utf-8",
        "body": "abcd",
        "truncated": True,
        "redirected": True,
    }


@pytest.mark.parametrize(
    "url",
    ["http://192.0.2.1/", "http://localhost/", "http://127.0.0.1:81/"],
)
def test_authorized_http_tool_rejects_non_allowlisted_destinations(url: str) -> None:
    adapter = AuthorizedHTTPToolAdapter(
        allowed_cidrs=["127.0.0.0/8"], allowed_ports=[80]
    )
    workspace = Path("/tmp")

    with pytest.raises((ValueError, PermissionError)):
        asyncio.run(
            adapter.execute({"url": url}, _context(workspace, workspace))
        )


def test_authorized_http_tool_rejects_methods_outside_explicit_set() -> None:
    adapter = AuthorizedHTTPToolAdapter(
        allowed_cidrs=["127.0.0.0/8"], methods=["GET"]
    )
    workspace = Path("/tmp")

    with pytest.raises(ValueError, match="method is not enabled"):
        asyncio.run(
            adapter.execute(
                {"method": "DELETE", "url": "http://127.0.0.1/"},
                _context(workspace, workspace),
            )
        )


def test_registered_network_plugin_still_requires_gateway_policy_authorization(
    tmp_path: Path,
) -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, text="authorized")

    adapter = AuthorizedHTTPToolAdapter(
        allowed_cidrs=["127.0.0.0/8"],
        transport=httpx.MockTransport(handler),
    )
    headers = {"Authorization": "Bearer gateway-token"}
    payload = {
        "name": "network.request",
        "tool_call_id": "network-call",
        "args": {"url": "http://127.0.0.1/"},
    }
    denied = TestClient(
        create_gateway_app(
            event_file=tmp_path / "denied.jsonl",
            workspace=tmp_path,
            task_dir=tmp_path,
            gateway_token="gateway-token",
            tool_adapters=[adapter],
        )
    ).post("/v1/tools/call", headers=headers, json=payload)
    assert denied.status_code == 403
    assert calls == []

    allowed = TestClient(
        create_gateway_app(
            event_file=tmp_path / "allowed.jsonl",
            workspace=tmp_path,
            task_dir=tmp_path,
            gateway_token="gateway-token",
            policy=GatewayPolicy(allowed_tools={"network.request"}),
            tool_adapters=[adapter],
        )
    ).post("/v1/tools/call", headers=headers, json=payload)
    assert allowed.status_code == 200
    assert allowed.json()["result"]["status_code"] == 200
    assert allowed.json()["evidence_refs"][0]["trust"] == "untrusted"
    assert len(calls) == 1

    blocked_payload = {
        **payload,
        "tool_call_id": "blocked-network-call",
        "args": {"url": "http://192.0.2.1/"},
    }
    blocked = TestClient(
        create_gateway_app(
            event_file=tmp_path / "blocked.jsonl",
            workspace=tmp_path,
            task_dir=tmp_path,
            gateway_token="gateway-token",
            policy=GatewayPolicy(allowed_tools={"network.request"}),
            tool_adapters=[adapter],
        )
    ).post("/v1/tools/call", headers=headers, json=blocked_payload)
    assert blocked.status_code == 403
    assert blocked.json()["detail"] == "tool adapter denied the request"
    assert len(calls) == 1
    blocked_events = [
        json.loads(line)
        for line in (tmp_path / "blocked.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert blocked_events[-1]["data"]["failure_class"] == "adapter_denied"

    invalid = TestClient(
        create_gateway_app(
            event_file=tmp_path / "invalid.jsonl",
            workspace=tmp_path,
            task_dir=tmp_path,
            gateway_token="gateway-token",
            policy=GatewayPolicy(allowed_tools={"network.request"}),
            tool_adapters=[adapter],
        )
    ).post(
        "/v1/tools/call",
        headers=headers,
        json={
            **payload,
            "tool_call_id": "invalid-network-call",
            "args": {"url": "http://localhost/"},
        },
    )
    assert invalid.status_code == 400
