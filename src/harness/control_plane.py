from __future__ import annotations

from collections.abc import Callable
from pathlib import Path

from fastapi import FastAPI
from starlette.responses import JSONResponse

from .api.coordination import build_coordination_router
from .api.jobs import build_jobs_router
from .api.registry import build_registry_router
from .api.runs import build_runs_router
from .coordination import CoordinationStore
from .queue import SQLiteQueue
from .world import SQLiteWorldRepository, WorldRepository

_MAX_CONTROL_REQUEST_BYTES = 2 * 1024 * 1024


class _RequestBodyLimitMiddleware:
    def __init__(self, app, *, max_bytes: int) -> None:
        self.app = app
        self.max_bytes = max_bytes

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        content_length = next(
            (value for name, value in scope.get("headers", ()) if name.lower() == b"content-length"),
            None,
        )
        if content_length is not None:
            try:
                declared_length = int(content_length)
            except ValueError:
                declared_length = 0
            if declared_length > self.max_bytes:
                response = JSONResponse(
                    {"detail": f"request body exceeds the {self.max_bytes}-byte limit"},
                    status_code=413,
                )
                await response(scope, receive, send)
                return

        chunks = bytearray()
        more_body = True
        while more_body:
            message = await receive()
            if message["type"] == "http.disconnect":
                return
            chunk = message.get("body", b"")
            if len(chunks) + len(chunk) > self.max_bytes:
                response = JSONResponse(
                    {"detail": f"request body exceeds the {self.max_bytes}-byte limit"},
                    status_code=413,
                )
                await response(scope, receive, send)
                return
            chunks.extend(chunk)
            more_body = message.get("more_body", False)

        delivered = False

        async def replay_body():
            nonlocal delivered
            if delivered:
                return {"type": "http.disconnect"}
            delivered = True
            return {"type": "http.request", "body": bytes(chunks), "more_body": False}

        await self.app(scope, replay_body, send)


def create_control_plane(
    *,
    queue_db: Path,
    runs_root: Path,
    benchmarks_root: Path | None = None,
    skills_root: Path | None = None,
    token: str | None = None,
    world_repository_factory: Callable[[Path], WorldRepository] = SQLiteWorldRepository,
) -> FastAPI:
    app = FastAPI(title="Red Harness Control Plane", version="0.6.0")
    app.add_middleware(_RequestBodyLimitMiddleware, max_bytes=_MAX_CONTROL_REQUEST_BYTES)

    queue = SQLiteQueue(queue_db)
    coordination = CoordinationStore(queue_db.with_name("coordination.db"))
    run_root = runs_root.resolve()
    benchmark_root = benchmarks_root.resolve() if benchmarks_root else None
    skill_root = skills_root.resolve() if skills_root else None

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    app.include_router(build_jobs_router(queue, token))
    app.include_router(
        build_registry_router(
            benchmark_root=benchmark_root,
            skill_root=skill_root,
            token=token,
        )
    )
    app.include_router(
        build_runs_router(
            run_root=run_root,
            skill_root=skill_root,
            token=token,
            world_repository_factory=world_repository_factory,
        )
    )
    app.include_router(
        build_coordination_router(
            coordination=coordination,
            run_root=run_root,
            skill_root=skill_root,
            token=token,
        )
    )
    return app
