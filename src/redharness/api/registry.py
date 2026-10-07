from __future__ import annotations

from pathlib import Path

from fastapi import APIRouter, Request

from ..execution import ExecutionCapabilities
from ..registry import scan_benchmarks
from ..skills import scan_skills
from .common import authorize


def build_registry_router(
    *,
    benchmark_root: Path | None,
    skill_root: Path | None,
    token: str | None,
) -> APIRouter:
    router = APIRouter()

    @router.get("/v1/capabilities")
    async def capabilities(request: Request) -> dict[str, bool]:
        authorize(request, token)
        return ExecutionCapabilities.detect().as_dict()

    @router.get("/v1/benchmarks")
    async def benchmarks(request: Request) -> list[dict]:
        authorize(request, token)
        return [] if benchmark_root is None else scan_benchmarks(benchmark_root)

    @router.get("/v1/skills")
    async def skills(request: Request) -> list[dict]:
        authorize(request, token)
        return [] if skill_root is None else scan_skills(skill_root)

    return router
