from __future__ import annotations

import json
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel, Field

from .policy import GatewayPolicy


class ToolCallRequest(BaseModel):
    name: str = Field(min_length=1)
    args: dict[str, Any] = Field(default_factory=dict)


@dataclass(frozen=True)
class ModelPricing:
    input_per_million_usd: float = 0.0
    output_per_million_usd: float = 0.0

    def cost(self, input_tokens: int, output_tokens: int) -> float:
        return (
            input_tokens * self.input_per_million_usd
            + output_tokens * self.output_per_million_usd
        ) / 1_000_000


class GatewayEventWriter:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def emit(self, event_type: str, **data: Any) -> None:
        payload = {"type": event_type, "data": data}
        line = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
        with self._lock, self.path.open("a", encoding="utf-8") as handle:
            handle.write(line + "\n")
            handle.flush()


def _authorize(request: Request, token: str | None) -> None:
    if token is None:
        return
    auth = request.headers.get("authorization", "")
    if auth != f"Bearer {token}":
        raise HTTPException(status_code=401, detail="invalid gateway token")


def _resolve_read_path(raw: str, *, workspace: Path, task_dir: Path) -> Path:
    path = Path(raw)
    candidate = path if path.is_absolute() else workspace / path
    resolved = candidate.resolve()
    allowed_roots = (workspace.resolve(), task_dir.resolve())
    if not any(resolved == root or root in resolved.parents for root in allowed_roots):
        raise HTTPException(status_code=403, detail="path is outside allowed read roots")
    return resolved


def _resolve_write_path(raw: str, *, workspace: Path) -> Path:
    path = Path(raw)
    candidate = path if path.is_absolute() else workspace / path
    parent = candidate.parent.resolve()
    root = workspace.resolve()
    if parent != root and root not in parent.parents:
        raise HTTPException(status_code=403, detail="path is outside writable workspace")
    return parent / candidate.name


def _upstream_url(base: str, path: str) -> str:
    normalized = base.rstrip("/")
    if normalized.endswith("/v1"):
        return normalized + path.removeprefix("/v1")
    return normalized + path


def _usage_values(usage: dict[str, Any]) -> tuple[int, int, int]:
    input_tokens = int(usage.get("prompt_tokens", usage.get("input_tokens", 0)) or 0)
    output_tokens = int(
        usage.get("completion_tokens", usage.get("output_tokens", 0)) or 0
    )
    total_tokens = int(
        usage.get("total_tokens", input_tokens + output_tokens)
        or input_tokens + output_tokens
    )
    return input_tokens, output_tokens, total_tokens


def _emit_usage(
    writer: GatewayEventWriter,
    pricing: ModelPricing,
    *,
    model: str,
    usage: dict[str, Any],
) -> None:
    input_tokens, output_tokens, total_tokens = _usage_values(usage)
    writer.emit(
        "model.usage",
        model=model,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        cost_usd=pricing.cost(input_tokens, output_tokens),
    )


def _stream_usage(line: str) -> dict[str, Any] | None:
    if not line.startswith("data:"):
        return None
    data = line[5:].strip()
    if not data or data == "[DONE]":
        return None
    try:
        payload = json.loads(data)
    except json.JSONDecodeError:
        return None
    if not isinstance(payload, dict):
        return None
    usage = payload.get("usage")
    return usage if isinstance(usage, dict) else None


def create_gateway_app(
    *,
    event_file: Path,
    workspace: Path,
    task_dir: Path,
    policy: GatewayPolicy | None = None,
    gateway_token: str | None = None,
    model_upstream: str | None = None,
    model_api_key: str | None = None,
    model_pricing: ModelPricing | None = None,
    http_transport: httpx.AsyncBaseTransport | None = None,
) -> FastAPI:
    app = FastAPI(title="Red Harness Gateway", version="0.4.0")
    writer = GatewayEventWriter(event_file)
    active_policy = policy or GatewayPolicy()
    pricing = model_pricing or ModelPricing()

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.post("/v1/tools/call")
    async def call_tool(request: Request, call: ToolCallRequest) -> dict[str, Any]:
        _authorize(request, gateway_token)
        if not active_policy.allows(call.name):
            writer.emit("tool.denied", tool=call.name)
            raise HTTPException(status_code=403, detail=f"tool denied: {call.name}")

        writer.emit("tool.call", tool=call.name)
        try:
            if call.name == "file.read":
                raw_path = str(call.args.get("path", ""))
                if not raw_path:
                    raise HTTPException(status_code=400, detail="file.read requires path")
                path = _resolve_read_path(raw_path, workspace=workspace, task_dir=task_dir)
                content = path.read_bytes()
                truncated = len(content) > active_policy.max_tool_output_bytes
                content = content[: active_policy.max_tool_output_bytes]
                result = {
                    "path": str(path),
                    "content": content.decode("utf-8", errors="replace"),
                    "truncated": truncated,
                }
            elif call.name == "file.write":
                raw_path = str(call.args.get("path", ""))
                content = str(call.args.get("content", ""))
                if not raw_path:
                    raise HTTPException(status_code=400, detail="file.write requires path")
                encoded = content.encode("utf-8")
                if len(encoded) > active_policy.max_file_write_bytes:
                    raise HTTPException(status_code=413, detail="file.write payload too large")
                path = _resolve_write_path(raw_path, workspace=workspace)
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(encoded)
                result = {"path": str(path), "bytes_written": len(encoded)}
            else:
                raise HTTPException(status_code=501, detail=f"tool not implemented: {call.name}")
        except HTTPException:
            raise
        except OSError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc

        writer.emit("tool.result", tool=call.name, ok=True)
        return {"ok": True, "result": result}

    @app.post("/v1/chat/completions", response_model=None)
    async def chat_completions(request: Request):
        _authorize(request, gateway_token)
        if model_upstream is None:
            raise HTTPException(status_code=503, detail="model upstream is not configured")

        payload = await request.json()
        if not isinstance(payload, dict):
            raise HTTPException(status_code=400, detail="request body must be an object")

        model = str(payload.get("model", ""))
        is_stream = bool(payload.get("stream"))
        writer.emit(
            "model.request",
            model=model,
            endpoint="/v1/chat/completions",
            stream=is_stream,
        )
        headers = {"content-type": "application/json"}
        if model_api_key:
            headers["authorization"] = f"Bearer {model_api_key}"

        upstream_url = _upstream_url(model_upstream, "/v1/chat/completions")

        if not is_stream:
            async with httpx.AsyncClient(transport=http_transport, timeout=120.0) as client:
                response = await client.post(upstream_url, json=payload, headers=headers)

            try:
                body = response.json()
            except ValueError:
                writer.emit("model.response", model=model, status_code=response.status_code)
                return JSONResponse(
                    status_code=response.status_code,
                    content={"error": {"message": "upstream returned non-JSON response"}},
                )

            writer.emit("model.response", model=model, status_code=response.status_code)
            if response.is_success and isinstance(body, dict):
                usage = body.get("usage")
                if isinstance(usage, dict):
                    _emit_usage(writer, pricing, model=model, usage=usage)
                else:
                    writer.emit("model.usage_missing", model=model, stream=False)

            return JSONResponse(status_code=response.status_code, content=body)

        stream_payload = dict(payload)
        stream_options = stream_payload.get("stream_options")
        if not isinstance(stream_options, dict):
            stream_options = {}
        stream_payload["stream_options"] = {**stream_options, "include_usage": True}

        client = httpx.AsyncClient(transport=http_transport, timeout=120.0)
        upstream_request = client.build_request(
            "POST",
            upstream_url,
            json=stream_payload,
            headers=headers,
        )
        response = await client.send(upstream_request, stream=True)

        if not response.is_success:
            body_bytes = await response.aread()
            await response.aclose()
            await client.aclose()
            writer.emit("model.response", model=model, status_code=response.status_code)
            try:
                body = json.loads(body_bytes)
            except (json.JSONDecodeError, UnicodeDecodeError):
                body = {"error": {"message": "upstream returned non-JSON response"}}
            return JSONResponse(status_code=response.status_code, content=body)

        async def relay():
            usage_seen = False
            buffered = ""
            try:
                async for chunk in response.aiter_bytes():
                    yield chunk
                    buffered += chunk.decode("utf-8", errors="ignore")
                    while "\n" in buffered:
                        line, buffered = buffered.split("\n", 1)
                        usage = _stream_usage(line.rstrip("\r"))
                        if usage is not None:
                            _emit_usage(writer, pricing, model=model, usage=usage)
                            usage_seen = True
                if buffered:
                    usage = _stream_usage(buffered.rstrip("\r"))
                    if usage is not None:
                        _emit_usage(writer, pricing, model=model, usage=usage)
                        usage_seen = True
            finally:
                writer.emit("model.response", model=model, status_code=response.status_code)
                if not usage_seen:
                    writer.emit("model.usage_missing", model=model, stream=True)
                await response.aclose()
                await client.aclose()

        return StreamingResponse(
            relay(),
            status_code=response.status_code,
            media_type="text/event-stream",
        )

    return app
