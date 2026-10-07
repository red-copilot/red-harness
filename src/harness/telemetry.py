from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any


def emit(event_type: str, **data: Any) -> None:
    """Emit one agent telemetry event when running under Red Harness."""
    target = os.environ.get("HARNESS_EVENT_FILE")
    if not target:
        return
    path = Path(target)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = {"type": event_type, "data": data}
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n")
        handle.flush()


def model_request(*, model: str | None = None, count: int = 1) -> None:
    emit("model.request", model=model, count=count)


def model_usage(
    *,
    input_tokens: int = 0,
    output_tokens: int = 0,
    total_tokens: int | None = None,
    cost_usd: float = 0.0,
    model: str | None = None,
    model_calls: int = 1,
) -> None:
    emit(
        "model.usage",
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens if total_tokens is not None else input_tokens + output_tokens,
        cost_usd=cost_usd,
        model=model,
        model_calls=model_calls,
    )


def tool_call(*, tool: str, count: int = 1) -> None:
    emit("tool.call", tool=tool, count=count)
