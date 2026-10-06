import json
from pathlib import Path

import httpx
from fastapi.testclient import TestClient

from redharness.gateway import ModelPricing, create_gateway_app
from redharness.policy import GatewayPolicy


def _events(path: Path) -> list[dict]:
    return [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def test_tool_gateway_auth_policy_and_paths(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_dir = tmp_path / "task"
    workspace.mkdir()
    task_dir.mkdir()
    (task_dir / "input.txt").write_text("task-data", encoding="utf-8")
    event_file = tmp_path / "events.jsonl"

    app = create_gateway_app(
        event_file=event_file,
        workspace=workspace,
        task_dir=task_dir,
        policy=GatewayPolicy(),
        gateway_token="gateway-secret",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer gateway-secret"}

    assert client.post(
        "/v1/tools/call",
        json={"name": "file.write", "args": {"path": "proof.txt", "content": "ok"}},
    ).status_code == 401

    written = client.post(
        "/v1/tools/call",
        headers=headers,
        json={"name": "file.write", "args": {"path": "proof.txt", "content": "ok"}},
    )
    assert written.status_code == 200
    assert (workspace / "proof.txt").read_text(encoding="utf-8") == "ok"

    read = client.post(
        "/v1/tools/call",
        headers=headers,
        json={"name": "file.read", "args": {"path": str(task_dir / "input.txt")}},
    )
    assert read.status_code == 200
    assert read.json()["result"]["content"] == "task-data"

    escaped = client.post(
        "/v1/tools/call",
        headers=headers,
        json={"name": "file.write", "args": {"path": "../escape.txt", "content": "no"}},
    )
    assert escaped.status_code == 403

    denied = client.post(
        "/v1/tools/call",
        headers=headers,
        json={"name": "shell.exec", "args": {"command": "id"}},
    )
    assert denied.status_code == 403

    event_types = [item["type"] for item in _events(event_file)]
    assert event_types.count("tool.call") == 3
    assert "tool.denied" in event_types


def test_model_proxy_replaces_credentials_and_records_usage(tmp_path: Path) -> None:
    event_file = tmp_path / "events.jsonl"

    def upstream(request: httpx.Request) -> httpx.Response:
        assert request.headers["authorization"] == "Bearer provider-secret"
        payload = json.loads(request.content)
        assert payload["model"] == "test-model"
        return httpx.Response(
            200,
            json={
                "id": "chatcmpl-test",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}}],
                "usage": {
                    "prompt_tokens": 100,
                    "completion_tokens": 50,
                    "total_tokens": 150,
                },
            },
        )

    app = create_gateway_app(
        event_file=event_file,
        workspace=tmp_path,
        task_dir=tmp_path,
        gateway_token="gateway-secret",
        model_upstream="https://model.example",
        model_api_key="provider-secret",
        model_pricing=ModelPricing(
            input_per_million_usd=1.0,
            output_per_million_usd=2.0,
        ),
        http_transport=httpx.MockTransport(upstream),
    )
    client = TestClient(app)

    response = client.post(
        "/v1/chat/completions",
        headers={"Authorization": "Bearer gateway-secret"},
        json={
            "model": "test-model",
            "messages": [{"role": "user", "content": "hello"}],
        },
    )
    assert response.status_code == 200

    events = _events(event_file)
    assert [item["type"] for item in events] == [
        "model.request",
        "model.response",
        "model.usage",
    ]
    usage = events[-1]["data"]
    assert usage["total_tokens"] == 150
    assert usage["cost_usd"] == 0.0002


def test_streaming_is_explicitly_rejected(tmp_path: Path) -> None:
    app = create_gateway_app(
        event_file=tmp_path / "events.jsonl",
        workspace=tmp_path,
        task_dir=tmp_path,
        gateway_token="secret",
        model_upstream="https://model.example",
    )
    client = TestClient(app)
    response = client.post(
        "/v1/chat/completions",
        headers={"Authorization": "Bearer secret"},
        json={"model": "m", "messages": [], "stream": True},
    )
    assert response.status_code == 400
