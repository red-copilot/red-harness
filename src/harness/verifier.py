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
    if task_dir.resolve() not in entrypoint.parents:
        raise VerifierError("python verifier entrypoint must remain inside the task directory")
    bwrap = shutil.which("bwrap")
    if bwrap is None:
        raise VerifierError("bubblewrap is required to sandbox Python verifiers")

    command = _python_verifier_sandbox_command(
        bwrap=bwrap,
        entrypoint=entrypoint,
        task_dir=task_dir,
        run_dir=run_dir,
    )
    env = {
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "HOME": "/tmp",
        "TMPDIR": "/tmp",
        "TMP": "/tmp",
        "TEMP": "/tmp",
        "PYTHONUTF8": "1",
        "PYTHONIOENCODING": "utf-8",
        "PYTHONDONTWRITEBYTECODE": "1",
        "PYTHONHASHSEED": str(seed & 0xFFFFFFFF),
        "HARNESS_TASK_ID": task.id,
        "HARNESS_TASK_DIR": "/task",
        "HARNESS_RUN_DIR": "/run/harness",
        "HARNESS_ENV_PROJECT": environment_project or "",
        "HARNESS_SEED": str(seed),
    }
    try:
        return subprocess.run(
            command,
            cwd="/",
            env=env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=task.verification.timeout,
            check=False,
            close_fds=True,
            start_new_session=True,
            preexec_fn=_verifier_resource_limits(task.verification.timeout),
        )
    except subprocess.TimeoutExpired as exc:
        raise VerifierError("Python verifier timed out") from exc
    except OSError as exc:
        raise VerifierError(f"Python verifier runtime unavailable: {type(exc).__name__}") from exc


def _python_verifier_sandbox_command(
    *,
    bwrap: str,
    entrypoint: Path,
    task_dir: Path,
    run_dir: Path,
) -> list[str]:
    prefix = Path(sys.prefix).resolve()
    base_prefix = Path(sys.base_prefix).resolve()
    executable = Path(sys.executable).absolute()
    try:
        executable_relative = executable.relative_to(prefix)
    except ValueError as exc:
        raise VerifierError(
            "Python executable must be contained by its installation prefix"
        ) from exc

    command = [
        bwrap,
        "--die-with-parent",
        "--new-session",
        "--unshare-user",
        "--unshare-pid",
        "--unshare-net",
        "--unshare-ipc",
        "--unshare-uts",
        "--cap-drop",
        "ALL",
        "--ro-bind",
        "/usr",
        "/usr",
        "--ro-bind-try",
        "/lib",
        "/lib",
        "--ro-bind-try",
        "/lib64",
        "/lib64",
        "--ro-bind-try",
        "/etc/ld.so.cache",
        "/etc/ld.so.cache",
        "--tmpfs",
        "/tmp",
        "--tmpfs",
        "/run",
    ]

    if prefix == Path("/usr") or Path("/usr") in prefix.parents:
        sandbox_executable = executable
    elif Path("/opt") in prefix.parents:
        command.extend(["--ro-bind-try", "/opt", "/opt"])
        sandbox_executable = executable
    elif Path("/opt") in base_prefix.parents:
        raise VerifierError(
            "Python virtual environments based under /opt must also live under /opt"
        )
    else:
        if base_prefix != Path("/usr") and Path("/usr") not in base_prefix.parents:
            raise VerifierError(
                "Python verifier sandbox supports base interpreters under /usr or /opt"
            )
        command.extend(["--dir", "/opt", "--ro-bind", str(prefix), "/opt/harness-python"])
        sandbox_executable = Path("/opt/harness-python") / executable_relative

    command.extend(
        [
            "--dev",
            "/dev",
            "--proc",
            "/proc",
            "--tmpfs",
            "/task",
            "--ro-bind",
            str(task_dir.resolve()),
            "/task",
            "--dir",
            "/run/harness",
            "--ro-bind",
            str(run_dir.resolve()),
            "/run/harness",
            "--chdir",
            "/task",
            "--",
            str(sandbox_executable),
            f"/task/{entrypoint.relative_to(task_dir.resolve()).as_posix()}",
        ]
    )
    return command


def _verifier_resource_limits(timeout: int):
    if os.name != "posix":
        return None

    def apply_limits() -> None:
        import resource

        cpu_limit = max(1, timeout)
        for limit_name, soft, hard in (
            ("RLIMIT_CPU", cpu_limit, cpu_limit + 1),
            ("RLIMIT_CORE", 0, 0),
            ("RLIMIT_FSIZE", 64 * 1024 * 1024, 64 * 1024 * 1024),
            ("RLIMIT_NOFILE", 128, 128),
        ):
            limit = getattr(resource, limit_name, None)
            if limit is not None:
                resource.setrlimit(limit, (soft, hard))

    return apply_limits


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
    ]
    if task.verification.runtime:
        command.extend(["--runtime", task.verification.runtime])
    command.extend(
        [
            "-v",
            f"{task_dir.resolve()}:/task:ro",
            "-v",
            f"{run_dir.resolve()}:/run/harness:ro",
            "-w",
            "/task",
            "-e",
            f"HARNESS_TASK_ID={task.id}",
            "-e",
            "HARNESS_TASK_DIR=/task",
            "-e",
            "HARNESS_RUN_DIR=/run/harness",
            "-e",
            f"HARNESS_ENV_PROJECT={environment_project or ''}",
            "-e",
            f"HARNESS_SEED={seed}",
            str(task.verification.image),
        ]
    )
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
    try:
        return subprocess.run(
            command,
            cwd=task_dir,
            capture_output=True,
            text=True,
            timeout=task.verification.timeout,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise VerifierError("Docker verifier timed out") from exc
    except OSError as exc:
        raise VerifierError(f"Docker verifier runtime unavailable: {type(exc).__name__}") from exc


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
            "runtime": task.verification.runtime,
        },
    )

    if task.verification.type == "python":
        if task.verification.network != "none":
            raise VerifierError(
                "Python verifiers cannot receive environment networking; use a Docker verifier"
            )
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
        raise VerifierError(f"verifier exited with {proc.returncode}: {proc.stderr.strip()}")

    result = _parse_result(proc.stdout)
    trace.emit(
        "grader.result",
        actor="grader",
        data={"success": result.success, "score": result.score},
    )
    return result, proc.stdout, proc.stderr
