from __future__ import annotations

import hashlib
import json
import time
import uuid
from datetime import UTC, datetime
from pathlib import Path

from . import __version__
from .agent import build_agent_adapter
from .budget import UsageMetrics
from .environment import build_environment
from .gateway_runtime import GatewayConfig, build_gateway_runtime
from .models import AgentSpec, TaskSpec
from .trace import TraceRecorder
from .verifier import run_verifier


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _new_run_id() -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    return f"run_{stamp}_{uuid.uuid4().hex[:8]}"


class Orchestrator:
    def __init__(self, runs_root: str | Path = ".redharness/runs") -> None:
        self.runs_root = Path(runs_root)

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
    ) -> dict:
        run_id = _new_run_id()
        run_dir = (self.runs_root / run_id).resolve()
        run_dir.mkdir(parents=True, exist_ok=False)
        task_dir = task_path.resolve().parent
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, task.id)
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
            },
        )

        try:
            handle = environment.start()
            if gateway_config is not None:
                if agent.type == "docker" and agent.network == "none":
                    raise ValueError(
                        "Docker Agent network:none is incompatible with per-run Gateway"
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
            agent_result = adapter.run(
                task,
                task_dir=task_dir,
                run_dir=run_dir,
                environment_project=handle.project_name,
                environment_network=handle.network_name,
                seed=seed,
                gateway_url=gateway_runtime.url if gateway_runtime else None,
                gateway_token=gateway_runtime.token if gateway_runtime else None,
                gateway_network=gateway_runtime.network_name if gateway_runtime else None,
            )
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
                "docker_access": bool(gateway_config and agent.type == "docker"),
                "mode": gateway_config.mode if gateway_config else None,
            },
            "metrics": metrics,
            "versions": {
                "harness": __version__,
                "task_sha256": _sha256(task_path),
                "agent_sha256": _sha256(agent_path),
            },
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
