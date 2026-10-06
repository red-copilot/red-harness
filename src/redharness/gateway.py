from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
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

    def emit(self, event_type: str, **data: Any) -> None:
        payload = {"type": event_type, "data": data}
        with self.path.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n")
            handle.flush()


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


def create_gateway_app(
    *,
    event_file: Path,
    workspace: Path,
    task_dir: Path,
    policy: GatewayPolicy | None = None,
    model_upstream: str | None = None,
    model_api_key: str | None = None,
    model_pricing: ModelPricing | None = None,
    http_transport: httpx.AsyncBaseTransport | None = None,
) -> FastAPI:
    app = FastAPI(title="Red Harness Gateway", version="0.3.0")
    writer = GatewayEventWriter(event_file)
    active_policy = policy or GatewayPolicy()
    pricing = model_pricing or ModelPricing()

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.post("/v1/tools/call")
    async def call_tool(call: ToolCallRequest) -> dict[str, Any]:
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

    @app.post("/v1/chat/completions")
    async def chat_completions(request: Request) -> JSONResponse:
        if model_upstream is None:
            raise HTTPException(status_code=503, detail="model upstream is not configured")

        payload = await request.json()
        if bool(payload.get("stream")):
            raise HTTPException(status_code=400, detail="streaming is not supported in v0.3")

        model = str(payload.get("model", ""))
        writer.emit("model.request", model=model, endpoint="/v1/chat/completions")
        headers = {"content-type": "application/json"}
        if model_api_key:
            headers["authorization"] = f"Bearer {model_api_key}"

        upstream_url = model_upstream.rstrip("/") + "/v1/chat/completions"
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
            usage = body.get("usage", {})
            if isinstance(usage, dict):
                input_tokens = int(usage.get("prompt_tokens", usage.get("input_tokens", 0)) or 0)
                output_tokens = int(
                    usage.get("completion_tokens", usage.get("output_tokens", 0)) or 0
                )
                total_tokens = int(
                    usage.get("total_tokens", input_tokens + output_tokens)
                    or input_tokens + output_tokens
                )
                writer.emit(
                    "model.usage",
                    model=model,
                    input_tokens=input_tokens,
                    output_tokens=output_tokens,
                    total_tokens=total_tokens,
                    cost_usd=pricing.cost(input_tokens, output_tokens),
                )

        return JSONResponse(status_code=response.status_code, content=body)

    return app
