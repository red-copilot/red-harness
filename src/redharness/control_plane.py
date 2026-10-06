from __future__ import annotations

from pathlib import Path
from typing import Callable

from fastapi import FastAPI

from .api.coordination import build_coordination_router
from .api.jobs import build_jobs_router
from .api.registry import build_registry_router
from .api.runs import build_runs_router
from .coordination import CoordinationStore
from .queue import SQLiteQueue
from .world import FileWorldRepository, WorldRepository


def create_control_plane(
    *,
    queue_db: Path,
    runs_root: Path,
    benchmarks_root: Path | None = None,
    skills_root: Path | None = None,
    token: str | None = None,
    world_repository_factory: Callable[[Path], WorldRepository] = FileWorldRepository,
) -> FastAPI:
    app = FastAPI(title="Red Harness Control Plane", version="0.6.0")

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
