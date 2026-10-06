from __future__ import annotations

import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path

from .models import EnvironmentSpec
from .trace import TraceRecorder


class EnvironmentError(RuntimeError):
    pass


@dataclass
class EnvironmentHandle:
    provider: str
    project_name: str | None = None


class EnvironmentProvider:
    def start(self) -> EnvironmentHandle:
        raise NotImplementedError

    def stop(self) -> None:
        raise NotImplementedError


class NoneEnvironment(EnvironmentProvider):
    def __init__(self, trace: TraceRecorder) -> None:
        self.trace = trace

    def start(self) -> EnvironmentHandle:
        self.trace.emit("environment.started", data={"provider": "none"})
        return EnvironmentHandle(provider="none")

    def stop(self) -> None:
        self.trace.emit("environment.finished", data={"provider": "none"})


class DockerComposeEnvironment(EnvironmentProvider):
    def __init__(self, spec: EnvironmentSpec, task_dir: Path, run_id: str, trace: TraceRecorder) -> None:
        if shutil.which("docker") is None:
            raise EnvironmentError("docker executable was not found")
        self.spec = spec
        self.task_dir = task_dir
        self.trace = trace
        self.project_name = ("rh_" + run_id.lower()).replace("-", "_")[:48]
        self.manifest = (task_dir / str(spec.manifest)).resolve()

    def _compose(self, *args: str, timeout: int = 300) -> subprocess.CompletedProcess[str]:
        command = [
            "docker", "compose", "-p", self.project_name,
            "-f", str(self.manifest), *args,
        ]
        result = subprocess.run(
            command,
            cwd=self.task_dir,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        if result.returncode != 0:
            raise EnvironmentError(
                f"docker compose {' '.join(args)} failed: {result.stderr.strip()}"
            )
        return result

    def start(self) -> EnvironmentHandle:
        if not self.manifest.is_file():
            raise EnvironmentError(f"compose manifest not found: {self.manifest}")
        self.trace.emit(
            "environment.started",
            data={"provider": "docker-compose", "manifest": str(self.manifest)},
        )
        self._compose("up", "-d", "--build")
        return EnvironmentHandle(provider="docker-compose", project_name=self.project_name)

    def stop(self) -> None:
        try:
            self._compose("down", "-v", "--remove-orphans", timeout=180)
        finally:
            self.trace.emit(
                "environment.finished",
                data={"provider": "docker-compose", "project_name": self.project_name},
            )


def build_environment(
    spec: EnvironmentSpec,
    *,
    task_dir: Path,
    run_id: str,
    trace: TraceRecorder,
) -> EnvironmentProvider:
    if spec.provider == "none":
        return NoneEnvironment(trace)
    if spec.provider == "docker-compose":
        return DockerComposeEnvironment(spec, task_dir, run_id, trace)
    raise EnvironmentError(f"unsupported environment provider: {spec.provider}")
