from __future__ import annotations

import asyncio
import hashlib
import json
import shutil
import sqlite3
import time
import uuid
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path

from . import __version__
from .agent import build_agent_adapter
from .budget import UsageMetrics
from .environment import build_environment
from .gateway_runtime import GatewayConfig, build_gateway_runtime
from .models import AgentSpec, TaskSpec
from .session import AgentObservation
from .trace import TraceRecorder
from .verifier import run_verifier
from .world import (
    Goal,
    SQLiteWorldRepository,
    WorldContextBuilder,
    WorldRepository,
    ingest_world_inbox,
)
from .world.live import WorldInboxCursor


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _new_run_id() -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    return f"run_{stamp}_{uuid.uuid4().hex[:8]}"


class Orchestrator:
    def __init__(
        self,
        runs_root: str | Path = ".harness/runs",
        *,
        world_repository_factory: Callable[[Path], WorldRepository] = SQLiteWorldRepository,
    ) -> None:
        self.runs_root = Path(runs_root)
        self.world_repository_factory = world_repository_factory

    def run(
        self,
        *,
        task: TaskSpec,
        task_path: Path,
        agent: AgentSpec,
        agent_path: Path,
        allow_host_agent: bool = False,
        seed: int = 0,
        gateway_config: GatewayConfig | None = None,
        resume_world_events: Path | None = None,
    ) -> dict:
        run_id = _new_run_id()
        run_dir = (self.runs_root / run_id).resolve()
        run_dir.mkdir(parents=True, exist_ok=False)
        task_dir = task_path.resolve().parent
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, task.id)
        world_event_path = run_dir / "world.events.jsonl"
        world_db_path = run_dir / "world.db"
        if resume_world_events is not None:
            source = resume_world_events.resolve()
            if not source.is_file():
                raise FileNotFoundError(f"resume world state not found: {source}")
            if source.suffix == ".db":
                with sqlite3.connect(source) as source_db, sqlite3.connect(
                    world_db_path
                ) as target_db:
                    source_db.backup(target_db)
            else:
                shutil.copyfile(source, world_event_path)
        world = self.world_repository_factory(world_event_path)
        root_goal = Goal(
            id=f"goal:{task.id}:objective",
            description=task.objective.description,
            status="active",
            priority=1.0,
            attributes={"task_id": task.id, "category": task.category},
        )
        world.upsert("goal", root_goal)
        context_builder = WorldContextBuilder()
        (run_dir / "world.context.txt").write_text(
            context_builder.render(world.snapshot, query=task.objective.description),
            encoding="utf-8",
        )
        started = time.monotonic()
        status = "running"
        usage = UsageMetrics()
        gateway_runtime = None
        environment = build_environment(
            task.environment, task_dir=task_dir, run_id=run_id, trace=trace
        )

        trace.emit(
            "run.started",
            data={
                "agent_id": agent.id,
                "seed": seed,
                "gateway_enabled": gateway_config is not None,
                "task_sha256": _sha256(task_path),
                "agent_sha256": _sha256(agent_path),
                "resumed_world": resume_world_events is not None,
            },
        )

        try:
            handle = environment.start()
            if gateway_config is not None:
                if agent.type in {"docker", "pi"} and agent.network == "none":
                    raise ValueError(
                        "Container Agent network:none is incompatible with per-run Gateway"
                    )
                gateway_runtime = build_gateway_runtime(
                    config=gateway_config,
                    run_dir=run_dir,
                    task_dir=task_dir,
                    run_id=run_id,
                    trace=trace,
                    agent_type=agent.type,
                )
                gateway_runtime.start()

            adapter = build_agent_adapter(
                agent,
                allow_host_agent=allow_host_agent,
                trace=trace,
            )
            run_kwargs = {
                "task": task,
                "task_dir": task_dir,
                "run_dir": run_dir,
                "environment_project": handle.project_name,
                "environment_network": handle.network_name,
                "seed": seed,
                "gateway_url": gateway_runtime.url if gateway_runtime else None,
                "gateway_token": gateway_runtime.token if gateway_runtime else None,
                "gateway_network": gateway_runtime.network_name if gateway_runtime else None,
            }
            world_inbox = WorldInboxCursor(
                run_dir / "world.inbox.jsonl",
                world,
                actor=f"agent:{agent.id}",
            )

            async def run_session():
                agent_session = await adapter.start_session(**run_kwargs)
                async for _event in agent_session.events():
                    before_revision = world.snapshot.revision
                    live_ingest = world_inbox.poll()
                    after_revision = world.snapshot.revision
                    if live_ingest.accepted or live_ingest.rejected:
                        trace.emit(
                            "world.ingested",
                            data={
                                "accepted": live_ingest.accepted,
                                "rejected": live_ingest.rejected,
                                "errors": [
                                    error.model_dump()
                                    for error in live_ingest.errors[:10]
                                ],
                                "revision_before": before_revision,
                                "revision_after": after_revision,
                                "live": True,
                            },
                        )
                    if after_revision != before_revision:
                        (run_dir / "world.context.txt").write_text(
                            context_builder.render(
                                world.snapshot,
                                query=task.objective.description,
                            ),
                            encoding="utf-8",
                        )
                        trace.emit(
                            "world.state.updated",
                            data={
                                "revision_before": before_revision,
                                "revision_after": after_revision,
                            },
                        )
                        await agent_session.observe(
                            AgentObservation(
                                type="world.state.updated",
                                data={
                                    "revision_before": before_revision,
                                    "revision_after": after_revision,
                                    "context_path": "world.context.txt",
                                },
                            )
                        )
                world_inbox.poll()
                return await agent_session.result()

            start_session = getattr(adapter, "start_session", None)
            if callable(start_session):
                agent_result = asyncio.run(run_session())
            else:
                agent_result = adapter.run(**run_kwargs)
            usage = agent_result.metrics

            if agent_result.timed_out:
                status = "timeout"
                verification = {
                    "success": False,
                    "score": 0.0,
                    "message": "agent exceeded wall-time budget",
                    "milestones": {},
                }
            elif agent_result.budget_exceeded:
                status = "budget_exceeded"
                verification = {
                    "success": False,
                    "score": 0.0,
                    "message": f"agent exceeded {agent_result.budget_exceeded}",
                    "milestones": {},
                }
            else:
                verified, vout, verr = run_verifier(
                    task,
                    task_dir=task_dir,
                    run_dir=run_dir,
                    environment_project=handle.project_name,
                    environment_network=handle.network_name,
                    trace=trace,
                    seed=seed,
                )
                (run_dir / "verifier.stdout.log").write_text(vout, encoding="utf-8")
                (run_dir / "verifier.stderr.log").write_text(verr, encoding="utf-8")
                verification = verified.model_dump()
                status = "finished"
        except Exception as exc:  # noqa: BLE001 - orchestrator boundary records all failures.
            status = "error"
            verification = {
                "success": False,
                "score": 0.0,
                "message": f"{type(exc).__name__}: {exc}",
                "milestones": {},
            }
            trace.emit(
                "run.error",
                data={"error_type": type(exc).__name__, "message": str(exc)},
            )
        finally:
            if gateway_runtime is not None:
                try:
                    gateway_runtime.stop()
                except Exception as exc:  # noqa: BLE001 - teardown must preserve run result.
                    trace.emit(
                        "gateway.error",
                        data={"error_type": type(exc).__name__, "message": str(exc)},
                    )
            try:
                environment.stop()
            except Exception as exc:  # noqa: BLE001 - teardown must not hide the run result.
                trace.emit(
                    "environment.error",
                    data={"error_type": type(exc).__name__, "message": str(exc)},
                )

        ingest_report = (
            world_inbox.poll()
            if "world_inbox" in locals()
            else ingest_world_inbox(run_dir / "world.inbox.jsonl", world, actor="agent")
        )
        trace.emit(
            "world.ingested",
            data={
                "accepted": ingest_report.accepted,
                "rejected": ingest_report.rejected,
                "errors": [error.model_dump() for error in ingest_report.errors[:10]],
            },
        )

        duration_ms = int((time.monotonic() - started) * 1000)
        metrics = {"duration_ms": duration_ms, **usage.as_dict()}
        result = {
            "run_id": run_id,
            "task_id": task.id,
            "agent_id": agent.id,
            "seed": seed,
            "status": status,
            "success": bool(verification["success"]),
            "score": float(verification["score"]),
            "message": verification.get("message"),
            "milestones": verification.get("milestones", {}),
            "gateway": {
                "enabled": gateway_config is not None,
                "model_proxy": bool(gateway_config and gateway_config.model_upstream),
                "docker_access": bool(gateway_config and agent.type in {"docker", "pi"}),
                "mode": gateway_config.mode if gateway_config else None,
            },
            "metrics": metrics,
            "versions": {
                "harness": __version__,
                "task_sha256": _sha256(task_path),
                "agent_sha256": _sha256(agent_path),
            },
        }
        root_goal.status = "completed" if result["success"] else "failed"
        root_goal.attributes.update(
            {"run_status": status, "score": result["score"], "success": result["success"]}
        )
        world.upsert("goal", root_goal)
        (run_dir / "world.context.txt").write_text(
            context_builder.render(world.snapshot),
            encoding="utf-8",
        )
        result["world"] = {
            "revision": world.snapshot.revision,
            "resumed": resume_world_events is not None,
        }
        (run_dir / "result.json").write_text(
            json.dumps(result, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        trace.emit(
            "run.finished",
            data={
                "status": status,
                "success": result["success"],
                "score": result["score"],
                "metrics": metrics,
            },
        )
        return result
