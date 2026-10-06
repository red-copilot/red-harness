from __future__ import annotations

import hashlib
import json
import time
import uuid
from datetime import UTC, datetime
from pathlib import Path

from .agent import CLIAdapter
from .environment import build_environment
from .models import AgentSpec, TaskSpec
from .trace import TraceRecorder
from .verifier import run_python_verifier


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
    ) -> dict:
        run_id = _new_run_id()
        run_dir = (self.runs_root / run_id).resolve()
        run_dir.mkdir(parents=True, exist_ok=False)
        task_dir = task_path.resolve().parent
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, task.id)
        started = time.monotonic()
        status = "running"
        environment = build_environment(
            task.environment, task_dir=task_dir, run_id=run_id, trace=trace
        )
        handle = None

        trace.emit(
            "run.started",
            data={
                "agent_id": agent.id,
                "task_sha256": _sha256(task_path),
                "agent_sha256": _sha256(agent_path),
            },
        )

        try:
            handle = environment.start()
            adapter = CLIAdapter(agent, allow_host=allow_host_agent, trace=trace)
            agent_result = adapter.run(
                task,
                task_dir=task_dir,
                run_dir=run_dir,
                timeout=task.budgets.wall_time,
                environment_project=handle.project_name,
            )
            (run_dir / "agent.stdout.log").write_text(agent_result.stdout, encoding="utf-8")
            (run_dir / "agent.stderr.log").write_text(agent_result.stderr, encoding="utf-8")

            if agent_result.timed_out:
                status = "timeout"
                verification = {
                    "success": False,
                    "score": 0.0,
                    "message": "agent exceeded wall-time budget",
                    "milestones": {},
                }
                trace.emit("budget.exceeded", data={"budget": "wall_time"})
            else:
                verified, vout, verr = run_python_verifier(
                    task,
                    task_dir=task_dir,
                    run_dir=run_dir,
                    environment_project=handle.project_name,
                    trace=trace,
                )
                (run_dir / "verifier.stdout.log").write_text(vout, encoding="utf-8")
                (run_dir / "verifier.stderr.log").write_text(verr, encoding="utf-8")
                verification = verified.model_dump()
                status = "finished"
        except Exception as exc:
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
            try:
                environment.stop()
            except Exception as exc:
                trace.emit(
                    "environment.error",
                    data={"error_type": type(exc).__name__, "message": str(exc)},
                )

        duration_ms = int((time.monotonic() - started) * 1000)
        result = {
            "run_id": run_id,
            "task_id": task.id,
            "agent_id": agent.id,
            "status": status,
            "success": bool(verification["success"]),
            "score": float(verification["score"]),
            "message": verification.get("message"),
            "milestones": verification.get("milestones", {}),
            "metrics": {"duration_ms": duration_ms},
            "versions": {
                "harness": "0.1.0",
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
                "duration_ms": duration_ms,
            },
        )
        return result
