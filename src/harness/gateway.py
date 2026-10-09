from __future__ import annotations

import asyncio
import hashlib
import json
import secrets
import threading
import time
import uuid
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel, Field

from .policy import GatewayPolicy
from .secureio import open_beneath, open_regular_file
from .tool_adapter import (
    ToolAdapter,
    ToolCallRegistry,
    ToolEvidenceRef,
    ToolExecutionContext,
    execute_tool_adapter,
)
from .trace import sanitize_observability_data


class ToolCallRequest(BaseModel):
    name: str = Field(min_length=1)
    args: dict[str, Any] = Field(default_factory=dict)
    tool_call_id: str | None = Field(default=None, min_length=1, max_length=256)


@dataclass(frozen=True)
class ModelPricing:
    input_per_million_usd: float = 0.0
    output_per_million_usd: float = 0.0

    def cost(self, input_tokens: int, output_tokens: int) -> float:
        return (
            input_tokens * self.input_per_million_usd + output_tokens * self.output_per_million_usd
        ) / 1_000_000


class GatewayEventWriter:
    def __init__(self, path: Path, *, trusted_path: Path | None = None) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.trusted_path = trusted_path
        if trusted_path is not None:
            trusted_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def emit(self, event_type: str, **data: Any) -> None:
        payload = {"type": event_type, "data": data}
        line = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
        with self._lock:
            if self.trusted_path is not None and self.trusted_path != self.path:
                with open_regular_file(self.trusted_path, "a") as trusted:
                    trusted.write(line + "\n")
                    trusted.flush()
            with open_regular_file(self.path, "a") as handle:
                handle.write(line + "\n")
                handle.flush()


def _authorize(request: Request, token: str | None) -> None:
    if token is None:
        return
    auth = request.headers.get("authorization", "")
    if not secrets.compare_digest(auth.encode(), f"Bearer {token}".encode()):
        raise HTTPException(status_code=401, detail="invalid gateway token")


def _resolve_read_path(raw: str, *, workspace: Path, task_dir: Path) -> tuple[Path, Path]:
    path = Path(raw)
    roots = (workspace.resolve(), task_dir.resolve())
    if path.is_absolute():
        for root in roots:
            try:
                relative = path.relative_to(root)
            except ValueError:
                continue
            if relative.parts and ".." not in relative.parts:
                return root, relative
        raise HTTPException(status_code=403, detail="path is outside allowed read roots")
    if not path.parts or ".." in path.parts:
        raise HTTPException(status_code=403, detail="path traversal is not allowed")
    return roots[0], path


def _resolve_write_path(raw: str, *, workspace: Path) -> tuple[Path, Path]:
    path = Path(raw)
    root = workspace.resolve()
    if path.is_absolute():
        try:
            path = path.relative_to(root)
        except ValueError as exc:
            raise HTTPException(
                status_code=403, detail="path is outside writable workspace"
            ) from exc
    if not path.parts or ".." in path.parts:
        raise HTTPException(status_code=403, detail="path traversal is not allowed")
    return root, path


def _upstream_url(base: str, path: str) -> str:
    normalized = base.rstrip("/")
    if normalized.endswith("/v1"):
        return normalized + path.removeprefix("/v1")
    return normalized + path


def _usage_values(usage: dict[str, Any]) -> tuple[int, int, int]:
    input_tokens = usage.get("prompt_tokens", usage.get("input_tokens", 0))
    output_tokens = usage.get("completion_tokens", usage.get("output_tokens", 0))
    input_tokens = 0 if input_tokens is None else input_tokens
    output_tokens = 0 if output_tokens is None else output_tokens
    if any(
        isinstance(value, bool) or not isinstance(value, int) or value < 0
        for value in (input_tokens, output_tokens)
    ):
        raise ValueError("upstream token counters are invalid")
    raw_total = usage.get("total_tokens")
    if raw_total is not None and (
        isinstance(raw_total, bool) or not isinstance(raw_total, int) or raw_total < 0
    ):
        raise ValueError("upstream total token counter is invalid")
    total_tokens = max(
        input_tokens + output_tokens,
        raw_total if raw_total is not None else input_tokens + output_tokens,
    )
    return input_tokens, output_tokens, max(total_tokens, input_tokens + output_tokens)


def _output_evidence_ref(output: Any) -> ToolEvidenceRef:
    encoded = json.dumps(output, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    digest = hashlib.sha256(encoded.encode("utf-8")).hexdigest()
    return ToolEvidenceRef(evidence_id=f"sha256:{digest}", sha256=digest)


def _emit_usage(
    writer: GatewayEventWriter,
    pricing: ModelPricing,
    *,
    model: str,
    usage: dict[str, Any],
) -> None:
    try:
        input_tokens, output_tokens, total_tokens = _usage_values(usage)
    except ValueError:
        writer.emit("model.usage_missing", model=model, reason="invalid_usage")
        return
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
    trusted_event_file: Path | None = None,
    workspace: Path,
    task_dir: Path,
    policy: GatewayPolicy | None = None,
    gateway_token: str | None = None,
    model_upstream: str | None = None,
    model_api_key: str | None = None,
    model_pricing: ModelPricing | None = None,
    http_transport: httpx.AsyncBaseTransport | None = None,
    tool_adapters: Sequence[ToolAdapter] = (),
) -> FastAPI:
    app = FastAPI(title="Red Harness Gateway", version="0.4.0")
    writer = GatewayEventWriter(event_file, trusted_path=trusted_event_file)
    call_registry = ToolCallRegistry(event_file)
    active_policy = policy or GatewayPolicy()
    pricing = model_pricing or ModelPricing()
    adapters: dict[str, ToolAdapter] = {}
    for adapter in tool_adapters:
        name = getattr(adapter, "tool_name", None)
        if not isinstance(name, str) or not name:
            raise ValueError("tool adapter must declare a non-empty tool_name")
        if name in {"file.read", "file.write"} or name in adapters:
            raise ValueError(f"duplicate or reserved tool adapter name: {name}")
        adapters[name] = adapter

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.post("/v1/tools/call")
    async def call_tool(request: Request, call: ToolCallRequest) -> dict[str, Any]:
        _authorize(request, gateway_token)
        if not active_policy.allows(call.name):
            writer.emit("tool.denied", tool=call.name, tool_call_id=call.tool_call_id)
            raise HTTPException(status_code=403, detail=f"tool denied: {call.name}")

        if call.name in adapters and call.tool_call_id is None:
            writer.emit("tool.denied", tool=call.name, failure_class="tool_call_id_required")
            raise HTTPException(status_code=400, detail="registered tools require tool_call_id")
        call_id = call.tool_call_id or f"tool_{uuid.uuid4().hex}"
        if not call_registry.claim(call_id):
            writer.emit(
                "tool.denied",
                tool=call.name,
                tool_call_id=call_id,
                failure_class="duplicate_tool_call_id",
            )
            raise HTTPException(status_code=409, detail="tool call ID was already used")
        started = time.monotonic()
        cancellation: asyncio.Event | None = None
        writer.emit("tool.call", tool=call.name, tool_call_id=call_id)
        try:
            if call.name == "file.read":
                raw_path = str(call.args.get("path", ""))
                if not raw_path:
                    raise HTTPException(status_code=400, detail="file.read requires path")
                root, relative = _resolve_read_path(
                    raw_path, workspace=workspace, task_dir=task_dir
                )
                with open_beneath(root, relative, "rb") as handle:
                    content = handle.read(active_policy.max_tool_output_bytes + 1)
                truncated = len(content) > active_policy.max_tool_output_bytes
                content = content[: active_policy.max_tool_output_bytes]
                result = {
                    "path": str(root / relative),
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
                root, relative = _resolve_write_path(raw_path, workspace=workspace)
                with open_beneath(root, relative, "w", create_parents=True) as handle:
                    handle.write(content)
                result = {"path": str(root / relative), "bytes_written": len(encoded)}
            elif call.name in adapters:
                cancellation = asyncio.Event()
                context = ToolExecutionContext(
                    call_id=call_id,
                    workspace=workspace,
                    task_dir=task_dir,
                    cancelled=cancellation,
                )
                adapted = await execute_tool_adapter(adapters[call.name], call.args, context)
                result = adapted.output
            else:
                raise HTTPException(status_code=501, detail=f"tool not implemented: {call.name}")
        except asyncio.CancelledError:
            if cancellation is not None:
                cancellation.set()
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="unknown",
                failure_class="cancelled",
                duration_ms=int((time.monotonic() - started) * 1000),
            )
            raise
        except HTTPException as exc:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="failed",
                failure_class="tool_http_error",
                duration_ms=int((time.monotonic() - started) * 1000),
                status_code=exc.status_code,
            )
            raise
        except PermissionError as exc:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="failed",
                failure_class="adapter_denied",
                duration_ms=int((time.monotonic() - started) * 1000),
            )
            raise HTTPException(status_code=403, detail="tool adapter denied the request") from exc
        except ValueError as exc:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="failed",
                failure_class="invalid_tool_arguments",
                duration_ms=int((time.monotonic() - started) * 1000),
            )
            raise HTTPException(status_code=400, detail="tool arguments are invalid") from exc
        except OSError as exc:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="failed",
                failure_class=type(exc).__name__,
                duration_ms=int((time.monotonic() - started) * 1000),
            )
            raise HTTPException(status_code=400, detail="filesystem tool failed") from exc
        except Exception as exc:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="unknown",
                failure_class=type(exc).__name__,
                duration_ms=int((time.monotonic() - started) * 1000),
            )
            raise HTTPException(status_code=502, detail="tool adapter failed") from exc

        elapsed_ms = int((time.monotonic() - started) * 1000)
        try:
            encoded_result = json.dumps(
                result, ensure_ascii=False, sort_keys=True, separators=(",", ":")
            ).encode("utf-8")
        except (TypeError, ValueError) as exc:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="unknown",
                failure_class="output_not_serializable",
                duration_ms=elapsed_ms,
            )
            raise HTTPException(status_code=500, detail="tool returned non-JSON output") from exc
        if len(encoded_result) > active_policy.max_tool_output_bytes:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="unknown",
                failure_class="output_limit",
                duration_ms=elapsed_ms,
            )
            raise HTTPException(status_code=413, detail="tool output exceeds configured limit")
        refs = (_output_evidence_ref(result),)
        if call.name in adapters:
            refs += adapted.evidence_refs
        if len(refs) > 100:
            writer.emit(
                "tool.result",
                tool=call.name,
                tool_call_id=call_id,
                ok=False,
                is_error=True,
                execution_status="unknown",
                failure_class="evidence_ref_limit",
                duration_ms=elapsed_ms,
            )
            raise HTTPException(status_code=413, detail="too many tool evidence references")
        metadata = (
            sanitize_observability_data(dict(adapted.metadata)) if call.name in adapters else {}
        )
        writer.emit(
            "tool.result",
            tool=call.name,
            tool_call_id=call_id,
            ok=True,
            is_error=False,
            execution_status="succeeded",
            duration_ms=elapsed_ms,
            evidence_refs=[ref.model_dump(mode="json") for ref in refs],
            execution_metadata=metadata,
        )
        return {
            "ok": True,
            "tool_call_id": call_id,
            "result": result,
            "execution": {
                "status": "succeeded",
                "duration_ms": elapsed_ms,
                "metadata": metadata,
            },
            "evidence_refs": [ref.model_dump(mode="json") for ref in refs],
        }

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
            try:
                async with httpx.AsyncClient(transport=http_transport, timeout=120.0) as client:
                    response = await client.post(upstream_url, json=payload, headers=headers)
            except httpx.HTTPError as exc:
                writer.emit(
                    "model.upstream_error",
                    model=model,
                    stage="request",
                    status_code=502,
                    error_type=type(exc).__name__,
                )
                return JSONResponse(
                    status_code=502,
                    content={"error": {"message": "model upstream unavailable"}},
                )

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
        try:
            response = await client.send(upstream_request, stream=True)
        except httpx.HTTPError as exc:
            await client.aclose()
            writer.emit(
                "model.upstream_error",
                model=model,
                stage="request",
                status_code=502,
                error_type=type(exc).__name__,
            )
            return JSONResponse(
                status_code=502,
                content={"error": {"message": "model upstream unavailable"}},
            )

        if not response.is_success:
            try:
                body_bytes = await response.aread()
            except httpx.HTTPError as exc:
                await response.aclose()
                await client.aclose()
                writer.emit(
                    "model.upstream_error",
                    model=model,
                    stage="response",
                    status_code=502,
                    error_type=type(exc).__name__,
                )
                return JSONResponse(
                    status_code=502,
                    content={"error": {"message": "model upstream unavailable"}},
                )
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
            stream_error = False
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
            except httpx.HTTPError as exc:
                stream_error = True
                writer.emit(
                    "model.upstream_error",
                    model=model,
                    stage="stream",
                    status_code=response.status_code,
                    error_type=type(exc).__name__,
                )
                yield b'data: {"error":{"message":"model upstream disconnected"}}\n\n'
            finally:
                writer.emit(
                    "model.response",
                    model=model,
                    status_code=response.status_code,
                    complete=not stream_error,
                )
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
