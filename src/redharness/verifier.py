from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

from .models import TaskSpec, VerificationResult
from .trace import TraceRecorder


class VerifierError(RuntimeError):
    pass


def run_python_verifier(
    task: TaskSpec,
    *,
    task_dir: Path,
    run_dir: Path,
    environment_project: str | None,
    trace: TraceRecorder,
    timeout: int = 120,
) -> tuple[VerificationResult, str, str]:
    entrypoint = (task_dir / task.verification.entrypoint).resolve()
    if not entrypoint.is_file():
        raise VerifierError(f"verifier not found: {entrypoint}")

    env = os.environ.copy()
    env.update(
        {
            "REDHARNESS_TASK_ID": task.id,
            "REDHARNESS_TASK_DIR": str(task_dir),
            "REDHARNESS_RUN_DIR": str(run_dir),
            "REDHARNESS_ENV_PROJECT": environment_project or "",
        }
    )

    trace.emit("grader.started", actor="grader", data={"entrypoint": str(entrypoint)})
    proc = subprocess.run(
        [sys.executable, str(entrypoint)],
        cwd=task_dir,
        env=env,
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )
    if proc.returncode != 0:
        raise VerifierError(
            f"verifier exited with {proc.returncode}: {proc.stderr.strip()}"
        )

    lines = [line for line in proc.stdout.splitlines() if line.strip()]
    if not lines:
        raise VerifierError("verifier produced no JSON result")

    try:
        payload = json.loads(lines[-1])
        result = VerificationResult.model_validate(payload)
    except Exception as exc:
        raise VerifierError(f"invalid verifier result: {exc}") from exc

    trace.emit(
        "grader.result",
        actor="grader",
        data={"success": result.success, "score": result.score},
    )
    return result, proc.stdout, proc.stderr
