from __future__ import annotations

import os
import subprocess
from dataclasses import dataclass
from pathlib import Path

from .models import AgentSpec, TaskSpec
from .trace import TraceRecorder


class AgentError(RuntimeError):
    pass


@dataclass
class AgentResult:
    returncode: int
    timed_out: bool
    stdout: str
    stderr: str


class CLIAdapter:
    """Trusted local-process adapter.

    This adapter is intentionally opt-in because it executes the configured command
    on the host. Hardened agents should use a sandboxed adapter in future releases.
    """

    def __init__(self, spec: AgentSpec, *, allow_host: bool, trace: TraceRecorder) -> None:
        if not allow_host:
            raise AgentError(
                "CLI agents execute on the host; pass --allow-host-agent only for trusted agents"
            )
        self.spec = spec
        self.trace = trace

    def run(
        self,
        task: TaskSpec,
        *,
        task_dir: Path,
        run_dir: Path,
        timeout: int,
        environment_project: str | None,
    ) -> AgentResult:
        env = os.environ.copy()
        env.update(self.spec.env)
        env.update(
            {
                "REDHARNESS_TASK_ID": task.id,
                "REDHARNESS_TASK_DIR": str(task_dir),
                "REDHARNESS_RUN_DIR": str(run_dir),
                "REDHARNESS_OBJECTIVE": task.objective.description,
                "REDHARNESS_ENV_PROJECT": environment_project or "",
            }
        )
        cwd = Path(self.spec.cwd).resolve() if self.spec.cwd else task_dir

        self.trace.emit(
            "agent.started",
            actor="agent",
            data={"agent_id": self.spec.id, "type": self.spec.type},
        )
        try:
            proc = subprocess.run(
                self.spec.command,
                cwd=cwd,
                env=env,
                capture_output=True,
                text=True,
                timeout=timeout,
                check=False,
            )
            result = AgentResult(
                returncode=proc.returncode,
                timed_out=False,
                stdout=proc.stdout,
                stderr=proc.stderr,
            )
        except subprocess.TimeoutExpired as exc:
            result = AgentResult(
                returncode=124,
                timed_out=True,
                stdout=exc.stdout or "",
                stderr=exc.stderr or "",
            )

        self.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
            },
        )
        return result
