from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

from pydantic import ValidationError

from .models import TaskSpec, VerificationResult
from .trace import TraceRecorder


class VerifierError(RuntimeError):
    pass


def _parse_result(stdout: str) -> VerificationResult:
    lines = [line for line in stdout.splitlines() if line.strip()]
    if not lines:
        raise VerifierError("verifier produced no JSON result")
    try:
        payload = json.loads(lines[-1])
        return VerificationResult.model_validate(payload)
    except (json.JSONDecodeError, ValidationError, TypeError, ValueError) as exc:
        raise VerifierError(f"invalid verifier result: {exc}") from exc


def _run_python_verifier(
    task: TaskSpec,
    *,
    task_dir: Path,
    run_dir: Path,
    environment_project: str | None,
    seed: int,
) -> subprocess.CompletedProcess[str]:
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
            "REDHARNESS_SEED": str(seed),
        }
    )
    return subprocess.run(
        [sys.executable, str(entrypoint)],
        cwd=task_dir,
        env=env,
        capture_output=True,
        text=True,
        timeout=task.verification.timeout,
        check=False,
    )


def build_docker_verifier_command(
    task: TaskSpec,
    *,
    task_dir: Path,
    run_dir: Path,
    environment_project: str | None,
    environment_network: str | None,
    seed: int,
) -> list[str]:
    if task.verification.type != "docker":
        raise VerifierError("docker verifier command requested for non-docker verifier")
    if not task.verification.image:
        raise VerifierError("docker verifier image is missing")
    if shutil.which("docker") is None:
        raise VerifierError("docker executable was not found")

    network = (
        environment_network
        if task.verification.network == "environment" and environment_network
        else "none"
    )
    command = [
        "docker",
        "run",
        "--rm",
        "--pull=never",
        "--read-only",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--pids-limit",
        "128",
        "--memory",
        "1g",
        "--cpus",
        "1",
        "--tmpfs",
        "/tmp:rw,nosuid,nodev,size=128m",
        "--network",
        network,
        "-v",
        f"{task_dir.resolve()}:/task:ro",
        "-v",
        f"{run_dir.resolve()}:/run/redharness:ro",
        "-w",
        "/task",
        "-e",
        f"REDHARNESS_TASK_ID={task.id}",
        "-e",
        "REDHARNESS_TASK_DIR=/task",
        "-e",
        "REDHARNESS_RUN_DIR=/run/redharness",
        "-e",
        f"REDHARNESS_ENV_PROJECT={environment_project or ''}",
        "-e",
        f"REDHARNESS_SEED={seed}",
        str(task.verification.image),
    ]
    command.extend(task.verification.command)
    return command


def _run_docker_verifier(
    task: TaskSpec,
    *,
    task_dir: Path,
    run_dir: Path,
    environment_project: str | None,
    environment_network: str | None,
    seed: int,
) -> subprocess.CompletedProcess[str]:
    command = build_docker_verifier_command(
        task,
        task_dir=task_dir,
        run_dir=run_dir,
        environment_project=environment_project,
        environment_network=environment_network,
        seed=seed,
    )
    return subprocess.run(
        command,
        cwd=task_dir,
        capture_output=True,
        text=True,
        timeout=task.verification.timeout,
        check=False,
    )


def run_verifier(
    task: TaskSpec,
    *,
    task_dir: Path,
    run_dir: Path,
    environment_project: str | None,
    environment_network: str | None,
    trace: TraceRecorder,
    seed: int,
) -> tuple[VerificationResult, str, str]:
    trace.emit(
        "grader.started",
        actor="grader",
        data={
            "type": task.verification.type,
            "network": task.verification.network,
        },
    )

    if task.verification.type == "python":
        proc = _run_python_verifier(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            environment_project=environment_project,
            seed=seed,
        )
    elif task.verification.type == "docker":
        proc = _run_docker_verifier(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            environment_project=environment_project,
            environment_network=environment_network,
            seed=seed,
        )
    else:
        raise VerifierError(f"unsupported verifier type: {task.verification.type}")

    if proc.returncode != 0:
        raise VerifierError(
            f"verifier exited with {proc.returncode}: {proc.stderr.strip()}"
        )

    result = _parse_result(proc.stdout)
    trace.emit(
        "grader.result",
        actor="grader",
        data={"success": result.success, "score": result.score},
    )
    return result, proc.stdout, proc.stderr
