from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import math
import re
import shutil
import signal
import sqlite3
import stat
import threading
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
from .runtime import bootstrap_solver
from .runtime.agent_workspace import (
    create_agent_workspace,
    prepare_agent_task_view,
    sync_agent_workspace,
)
from .runtime.checkpoint import (
    load_recovery_state,
    run_action_owner,
    save_session_checkpoint,
)
from .runtime.evidence import persist_objective_verdict
from .runtime.lifecycle import RunLifecycle, resolve_run_status
from .runtime.projections import finalize_world_goal, network_result, world_result
from .runtime.results import persist_run_result
from .runtime.solver_profile import (
    DEFAULT_SOLVER_PROFILE,
    SolverProfile,
    solver_profile_settings,
)
from .secureio import read_regular_text
from .session import AgentObservation
from .trace import TraceRecorder
from .verifier import VerifierError, run_verifier
from .world import Goal, SQLiteWorldRepository, WorldRepository

_logger = logging.getLogger(__name__)


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _new_run_id() -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    return f"run_{stamp}_{uuid.uuid4().hex[:8]}"


def _sum_usage(previous: UsageMetrics, current: dict[str, int | float]) -> dict[str, int | float]:
    return {
        field: getattr(previous, field) + current.get(field, 0)
        for field in UsageMetrics.__dataclass_fields__
    }


def _remaining_budget_task(task: TaskSpec, started_at: datetime, used: UsageMetrics) -> TaskSpec:
    elapsed = max(0, math.ceil((datetime.now(UTC) - started_at).total_seconds()))
    mapping = {
        "max_tokens": used.total_tokens,
        "max_model_calls": used.model_calls,
        "max_tool_calls": used.tool_calls,
        "max_cost_usd": used.cost_usd,
    }
    remaining: dict[str, int | float | None] = {
        "wall_time": task.budgets.wall_time - elapsed,
    }
    for field, spent in mapping.items():
        limit = getattr(task.budgets, field)
        remaining[field] = None if limit is None else limit - spent
    exhausted = [field for field, value in remaining.items() if value is not None and value <= 0]
    if exhausted:
        raise RuntimeError(f"resume budget exhausted: {', '.join(exhausted)}")
    return task.model_copy(
        update={"budgets": task.budgets.model_copy(update=remaining)}
    )


class RunInterrupted(Exception):
    """Raised for a graceful process signal so run-owned resources can close."""


class Orchestrator:
    def __init__(
        self,
        runs_root: str | Path = ".harness/runs",
        *,
        world_repository_factory: Callable[[Path], WorldRepository] = SQLiteWorldRepository,
        skills_root: str | Path = "skills",
    ) -> None:
        self.runs_root = Path(runs_root)
        self.world_repository_factory = world_repository_factory
        self.skills_root = Path(skills_root)

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
        resume_run_dir: Path | None = None,
        run_id: str | None = None,
        solver_profile: SolverProfile = DEFAULT_SOLVER_PROFILE,
    ) -> dict:
        if threading.current_thread() is not threading.main_thread():
            return self._run(
                task=task,
                task_path=task_path,
                agent=agent,
                agent_path=agent_path,
                allow_host_agent=allow_host_agent,
                seed=seed,
                gateway_config=gateway_config,
                resume_world_events=resume_world_events,
                resume_run_dir=resume_run_dir,
                run_id=run_id,
                solver_profile=solver_profile,
            )

        previous_handlers: dict[int, object] = {}
        received_signal = False

        def interrupt(signum, _frame) -> None:
            nonlocal received_signal
            if received_signal:
                return
            received_signal = True
            signal_name = signal.Signals(signum).name
            raise RunInterrupted(f"received {signal_name}")

        for signum in (signal.SIGINT, signal.SIGTERM):
            previous_handlers[signum] = signal.signal(signum, interrupt)
        try:
            return self._run(
                task=task,
                task_path=task_path,
                agent=agent,
                agent_path=agent_path,
                allow_host_agent=allow_host_agent,
                seed=seed,
                gateway_config=gateway_config,
                resume_world_events=resume_world_events,
                resume_run_dir=resume_run_dir,
                run_id=run_id,
                solver_profile=solver_profile,
            )
        finally:
            for signum, handler in previous_handlers.items():
                signal.signal(signum, handler)

    def _run(
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
        resume_run_dir: Path | None = None,
        run_id: str | None = None,
        solver_profile: SolverProfile = DEFAULT_SOLVER_PROFILE,
    ) -> dict:
        solver_profile = SolverProfile(solver_profile)
        solver_profile_settings(solver_profile)
        if resume_run_dir is not None and resume_world_events is not None:
            raise ValueError("resume_run_dir and resume_world_events are mutually exclusive")
        is_resume = resume_run_dir is not None
        if is_resume:
            if run_id is not None:
                raise ValueError("run_id cannot be set when resuming an existing run")
            run_dir = self._validate_resume_directory(resume_run_dir)
            run_id = run_dir.name
            if agent.type != "pi" or agent.pi is None or agent.pi.mode != "rpc":
                raise ValueError("only Pi RPC runs can resume an agent session")
            workspace_stat = (run_dir / "agent-workspace").stat(follow_symlinks=False)
            if not stat.S_ISDIR(workspace_stat.st_mode):
                raise ValueError("agent workspace must be a real directory")
            agent_workspace = run_dir / "agent-workspace"
            started_trace = self._read_start_event(run_dir / "trace.jsonl")
            if started_trace.get("task_sha256") != _sha256(task_path):
                raise ValueError("resume task hash does not match original run")
            if started_trace.get("agent_sha256") != _sha256(agent_path):
                raise ValueError("resume agent hash does not match original run")
            if started_trace.get("task_id") != task.id:
                raise ValueError("resume task ID does not match original run")
            original_profile = started_trace.get(
                "solver_profile", DEFAULT_SOLVER_PROFILE.value
            )
            if original_profile != solver_profile.value:
                raise ValueError("resume solver profile does not match original run")
        else:
            run_id = run_id or _new_run_id()
            if not re.fullmatch(r"run_[A-Za-z0-9_-]{1,128}", run_id):
                raise ValueError("invalid run ID")
            run_dir = (self.runs_root / run_id).resolve()
            run_dir.mkdir(parents=True, exist_ok=False)
            agent_workspace = create_agent_workspace(run_dir)
        task_dir = task_path.resolve().parent
        agent_task_dir = (
            prepare_agent_task_view(
                task_dir=task_dir,
                run_root=run_dir,
                verifier_entrypoint=task.verification.entrypoint,
            )
            if agent.type in {"docker", "pi"}
            else None
        )
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, task.id)
        world_event_path = run_dir / "world.events.jsonl"
        world_db_path = run_dir / "world.db"
        if resume_world_events is not None:
            source = resume_world_events.resolve()
            if not source.is_file():
                raise FileNotFoundError(f"resume world state not found: {source}")
            if source.suffix == ".db":
                with (
                    sqlite3.connect(source) as source_db,
                    sqlite3.connect(world_db_path) as target_db,
                ):
                    source_db.backup(target_db)
            else:
                shutil.copyfile(source, world_event_path)
        root_goal = Goal(
            id=f"goal:{task.id}:objective",
            description=task.objective.description,
            status="active",
            priority=1.0,
            attributes={"task_id": task.id, "category": task.category},
        )
        runtime = bootstrap_solver(
            run_dir=run_dir,
            trace=trace,
            goal=root_goal,
            actor=f"agent:{agent.id}",
            context_query=task.objective.description,
            skills_root=self.skills_root,
            world_repository_factory=self.world_repository_factory,
            budget_limits=task.budgets.model_dump(exclude_none=True),
            agent_workspace=agent_workspace,
            resume_existing=is_resume,
            solver_profile=solver_profile,
        )
        world = runtime.world
        context_builder = runtime.context_builder
        progress = runtime.progress
        solver_loop = runtime.loop
        recovery_checkpoint = None
        recovered_usage = UsageMetrics()
        session_task = task
        if is_resume:
            recovery_checkpoint, restored_progress = load_recovery_state(
                run_dir, current_world_revision=world.snapshot.revision
            )
            if restored_progress.reconciled_actions:
                raise RuntimeError("reconciled action history prevents automatic session resume")
            if not recovery_checkpoint.agent_session_id:
                raise RuntimeError("checkpoint has no resumable Pi session ID")
            solver_loop.processed_event_ids.update(recovery_checkpoint.processed_event_ids)
            recovered_usage = UsageMetrics(
                input_tokens=int(recovery_checkpoint.budget_used.get("input_tokens", 0)),
                output_tokens=int(recovery_checkpoint.budget_used.get("output_tokens", 0)),
                total_tokens=int(recovery_checkpoint.budget_used.get("total_tokens", 0)),
                model_calls=int(recovery_checkpoint.budget_used.get("model_calls", 0)),
                tool_calls=int(recovery_checkpoint.budget_used.get("tool_calls", 0)),
                cost_usd=float(recovery_checkpoint.budget_used.get("cost_usd", 0)),
            )
            session_task = _remaining_budget_task(task, restored_progress.started_at, recovered_usage)
        task_verifier_authority = runtime.verifier_registry.register("task_verifier")
        progress_path = run_dir / "progress.json"
        sync_agent_workspace(run_dir=run_dir, workspace=agent_workspace)
        lifecycle = RunLifecycle()
        lifecycle.transition("running")
        status = "running"
        usage = UsageMetrics()
        gateway_runtime = None
        environment = build_environment(
            task.environment,
            task_dir=task_dir,
            run_id=run_id,
            trace=trace,
            resume_existing=is_resume,
        )

        start_event_data = {
                "agent_id": agent.id,
                "seed": seed,
                "gateway_enabled": gateway_config is not None,
                "task_sha256": _sha256(task_path),
                "agent_sha256": _sha256(agent_path),
                "resumed_world": resume_world_events is not None,
                "solver_profile": solver_profile.value,
            }
        if is_resume:
            trace.emit("run.resumed", data={"agent_session_id": recovery_checkpoint.agent_session_id})
        else:
            trace.emit("run.started", data=start_event_data)

        run_phase = "environment_startup"
        try:
            handle = environment.start()
            run_phase = "gateway_startup"
            if gateway_config is not None:
                if agent.type in {"docker", "pi"} and agent.network == "none":
                    raise ValueError(
                        "Container Agent network:none is incompatible with per-run Gateway"
                    )
                gateway_runtime = build_gateway_runtime(
                    config=gateway_config,
                    run_dir=agent_workspace,
                    task_dir=(
                        agent_task_dir
                        if agent.type in {"docker", "pi"} and agent_task_dir is not None
                        else task_dir
                    ),
                    run_id=run_id,
                    trace=trace,
                    agent_type=agent.type,
                )
                gateway_runtime.start()

            run_phase = "agent_execution"
            adapter = build_agent_adapter(
                agent,
                allow_host_agent=allow_host_agent,
                trace=trace,
            )
            run_kwargs = {
                "task": session_task,
                "task_dir": task_dir,
                "agent_task_dir": agent_task_dir,
                "run_dir": agent_workspace,
                "environment_project": handle.project_name,
                "environment_network": handle.network_name,
                "seed": seed,
                "gateway_url": gateway_runtime.url if gateway_runtime else None,
                "gateway_token": gateway_runtime.token if gateway_runtime else None,
                "gateway_network": gateway_runtime.network_name if gateway_runtime else None,
            }
            pi_rpc_gateway = bool(
                gateway_runtime and agent.type == "pi" and agent.pi and agent.pi.mode == "rpc"
            )
            if pi_rpc_gateway:
                run_kwargs["gateway_usage_path"] = gateway_runtime.usage_event_path
            if is_resume and pi_rpc_gateway:
                run_kwargs["resume_gateway_event_offset"] = (
                    recovery_checkpoint.gateway_event_offset
                )
            if is_resume:
                run_kwargs["resume_session_id"] = recovery_checkpoint.agent_session_id

            async def run_session():
                agent_session = await adapter.start_session(**run_kwargs)
                try:
                    await save_session_checkpoint(
                        run_dir=run_dir,
                        session=agent_session,
                        world_revision=world.snapshot.revision,
                        progress=progress,
                        plan_revision=solver_loop.planned_world_revision,
                        budget_used=_sum_usage(
                            recovered_usage,
                            getattr(agent_session, "usage_metrics", {}),
                        ),
                        processed_event_ids=solver_loop.processed_event_ids,
                    )
                    startup_decision = solver_loop.decide(force_replan=True)
                    if startup_decision.action.value == "stop":
                        trace.emit(
                            "solver.decision",
                            actor="harness",
                            data={"action": "stop", "reason": startup_decision.reason},
                        )
                        await agent_session.observe(
                            AgentObservation(
                                type="solver.stop", data={"reason": startup_decision.reason}
                            )
                        )
                        await agent_session.close(f"solver-stop:{startup_decision.reason}")
                        result = await agent_session.result()
                        await save_session_checkpoint(
                            run_dir=run_dir,
                            session=agent_session,
                            world_revision=world.snapshot.revision,
                            progress=progress,
                            plan_revision=solver_loop.planned_world_revision,
                            budget_used=_sum_usage(recovered_usage, result.metrics.as_dict()),
                            processed_event_ids=solver_loop.processed_event_ids,
                        )
                        return result
                    else:
                        await solver_loop.maybe_replan(agent_session, force=True)
                    async for event in agent_session.events():
                        decision = await solver_loop.process_event(agent_session, event)
                        sync_agent_workspace(run_dir=run_dir, workspace=agent_workspace)
                        await save_session_checkpoint(
                            run_dir=run_dir,
                            session=agent_session,
                            world_revision=world.snapshot.revision,
                            progress=progress,
                            plan_revision=solver_loop.planned_world_revision,
                            budget_used=_sum_usage(
                                recovered_usage,
                                getattr(agent_session, "usage_metrics", {}),
                            ),
                            processed_event_ids=solver_loop.processed_event_ids,
                        )
                        if decision.action.value == "stop":
                            await agent_session.close(f"solver-stop:{decision.reason}")
                            break
                    result = await agent_session.result()
                    await save_session_checkpoint(
                        run_dir=run_dir,
                        session=agent_session,
                        world_revision=world.snapshot.revision,
                        progress=progress,
                        plan_revision=solver_loop.planned_world_revision,
                        budget_used=_sum_usage(recovered_usage, result.metrics.as_dict()),
                        processed_event_ids=solver_loop.processed_event_ids,
                    )
                    return result
                except BaseException:
                    close = getattr(agent_session, "close", None)
                    if callable(close):
                        try:
                            await close("harness_run_error")
                        except Exception as close_exc:  # noqa: BLE001 - preserve original failure
                            trace.emit(
                                "agent.close_error",
                                data={"error_type": type(close_exc).__name__,
                                      "message": str(close_exc)},
                            )
                    raise

            start_session = getattr(adapter, "start_session", None)
            with run_action_owner(run_dir):
                if callable(start_session):
                    agent_result = asyncio.run(run_session())
                else:
                    agent_result = adapter.run(**run_kwargs)
            usage = UsageMetrics(**_sum_usage(recovered_usage, agent_result.metrics.as_dict()))

            if agent_result.timed_out:
                status = resolve_run_status(timed_out=True, budget_exceeded=False)
                verification = {
                    "success": False,
                    "score": 0.0,
                    "message": "agent exceeded wall-time budget",
                    "milestones": {},
                }
            elif agent_result.budget_exceeded:
                status = resolve_run_status(timed_out=False, budget_exceeded=True)
                verification = {
                    "success": False,
                    "score": 0.0,
                    "message": f"agent exceeded {agent_result.budget_exceeded}",
                    "milestones": {},
                }
            else:
                lifecycle.transition("verifying")
                run_phase = "verification"
                try:
                    verified, vout, verr = run_verifier(
                        task,
                        task_dir=task_dir,
                        run_dir=agent_workspace,
                        environment_project=handle.project_name,
                        environment_network=handle.network_name,
                        trace=trace,
                        seed=seed,
                    )
                except VerifierError as exc:
                    verification = {
                        "success": False,
                        "score": 0.0,
                        "message": "task verifier unavailable",
                        "milestones": {},
                    }
                    persist_objective_verdict(
                        run_dir=run_dir,
                        run_id=run_id,
                        world_revision=world.snapshot.revision,
                        producer_authority=task_verifier_authority,
                        registry=runtime.verifier_registry,
                        progress=progress,
                        trace=trace,
                        status="unavailable",
                        failure_type=type(exc).__name__,
                    )
                    progress.write(progress_path)
                    status = "error"
                else:
                    (run_dir / "verifier.stdout.log").write_text(vout, encoding="utf-8")
                    (run_dir / "verifier.stderr.log").write_text(verr, encoding="utf-8")
                    verification = verified.model_dump()
                    persist_objective_verdict(
                        run_dir=run_dir,
                        run_id=run_id,
                        world_revision=world.snapshot.revision,
                        producer_authority=task_verifier_authority,
                        registry=runtime.verifier_registry,
                        progress=progress,
                        trace=trace,
                        success=verified.success,
                        score=verified.score,
                        milestones=verified.milestones,
                    )
                    progress.write(progress_path)
                    status = resolve_run_status(timed_out=False, budget_exceeded=False)
        except Exception as exc:  # noqa: BLE001 - orchestrator boundary records all failures.
            cancelled = isinstance(exc, RunInterrupted)
            if not lifecycle.terminal:
                lifecycle.settle(succeeded=False, cancelled=cancelled)
            status = "cancelled" if cancelled else "error"
            failure_class = (
                "cancelled" if cancelled else
                "environment_failure" if run_phase == "environment_startup" else
                "gateway_failure" if run_phase == "gateway_startup" else
                "solver_failure" if run_phase == "agent_execution" else
                "harness_failure"
            )
            verification = {
                "success": False,
                "score": 0.0,
                # Exception messages from providers, containers and user code
                # can contain credentials or other sensitive input. Keep the
                # durable result useful for classification without persisting
                # untrusted exception text into run artifacts.
                "message": str(exc)
                if cancelled
                else f"{type(exc).__name__} (details omitted)",
                "milestones": {},
            }
            trace.emit(
                "run.error",
                data={"error_type": type(exc).__name__, "message": str(exc)},
            )
        finally:
            cleanup_failures: list[tuple[str, Exception]] = []
            if gateway_runtime is not None:
                try:
                    gateway_runtime.stop()
                except Exception as exc:  # noqa: BLE001 - teardown must preserve run result.
                    cleanup_failures.append(("gateway", exc))
            try:
                environment.stop(preserve_state=is_resume and status == "error")
            except Exception as exc:  # noqa: BLE001 - teardown must not hide the run result.
                cleanup_failures.append(("environment", exc))
            # Resource cleanup has priority over trace reporting. A full or
            # unavailable trace filesystem must not prevent later owners from
            # receiving their stop call.
            for owner, exc in cleanup_failures:
                try:
                    trace.emit(
                        f"{owner}.error",
                        data={"error_type": type(exc).__name__, "message": str(exc)},
                    )
                except Exception as report_exc:  # noqa: BLE001 - trace may be unavailable.
                    _logger.error(
                        "could not record %s cleanup failure in run trace (%s)",
                        owner,
                        type(report_exc).__name__,
                    )

        solver_loop.finish_ingest()

        if not lifecycle.terminal:
            lifecycle.settle(
                succeeded=bool(verification["success"]),
                exhausted=status in {"timeout", "budget_exceeded"},
            )

        duration_ms = max(
            0, int((datetime.now(UTC) - progress.started_at).total_seconds() * 1000)
        )
        metrics = {"duration_ms": duration_ms, **usage.as_dict()}
        if "failure_class" not in locals():
            verdict = progress.objective_verdict
            if status in {"timeout", "budget_exceeded"}:
                failure_class = "solver_budget_exhaustion"
            elif isinstance(verdict, dict) and verdict.get("status") == "unavailable":
                failure_class = "verifier_unavailable"
            elif status == "cancelled":
                failure_class = "cancelled"
            elif status == "error":
                failure_class = "harness_failure"
            elif verification["success"]:
                failure_class = "success"
            else:
                failure_class = "solver_failure"
        result = {
            "schema_version": "harness/result/v2",
            "run_id": run_id,
            "task_id": task.id,
            "agent_id": agent.id,
            "seed": seed,
            "status": status,
            "failure_class": failure_class,
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
            "progress": progress.model_dump(mode="json"),
            "network": network_result(agent),
            "versions": {
                "harness": __version__,
                "task_sha256": _sha256(task_path),
                "agent_sha256": _sha256(agent_path),
            },
        }
        if is_resume and status == "error":
            world.flush()
            trace.emit(
                "run.interrupted",
                data={"message": result["message"], "checkpoint_preserved": True},
            )
            return result
        finalize_world_goal(
            world=world,
            goal=root_goal,
            context_builder=context_builder,
            run_dir=run_dir,
            status=status,
            success=result["success"],
            score=result["score"],
        )
        world.flush()
        result["world"] = world_result(world=world, solver_loop=solver_loop, agent_id=agent.id)
        result["world"]["resumed"] = resume_world_events is not None
        persist_run_result(run_dir=run_dir, trace=trace, result=result)
        return result

    def _validate_resume_directory(self, candidate: Path) -> Path:
        runs_root = self.runs_root.resolve()
        if not runs_root.is_dir():
            raise ValueError("runs root does not exist")
        path = Path(candidate)
        if path.is_symlink():
            raise ValueError("resume run directory cannot be a symlink")
        resolved = path.resolve(strict=True)
        if resolved.parent != runs_root or not resolved.is_dir():
            raise ValueError("resume run must be a direct child of runs root")
        if (resolved / "result.json").exists():
            raise ValueError("terminal run cannot be resumed")
        trace_path = resolved / "trace.jsonl"
        trace_stat = trace_path.stat(follow_symlinks=False)
        if not stat.S_ISREG(trace_stat.st_mode):
            raise ValueError("run trace must be a regular file")
        for line in read_regular_text(trace_path, max_bytes=64 * 1024 * 1024).splitlines():
            event = json.loads(line)
            if event.get("type") == "run.finished":
                raise ValueError("terminal run cannot be resumed")
        return resolved

    @staticmethod
    def _read_start_event(trace_path: Path) -> dict:
        for line in read_regular_text(trace_path, max_bytes=64 * 1024 * 1024).splitlines():
            event = json.loads(line)
            if event.get("type") == "run.started":
                data = event.get("data")
                if not isinstance(data, dict):
                    break
                return {**data, "task_id": event.get("task_id")}
        raise ValueError("run trace has no valid run.started event")
