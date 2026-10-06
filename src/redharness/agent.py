from __future__ import annotations

import os
import shutil
import signal
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path

from .budget import BudgetMonitor, UsageMetrics
from .models import AgentSpec, TaskSpec
from .trace import TraceRecorder


class AgentError(RuntimeError):
    pass


@dataclass
class AgentResult:
    returncode: int
    timed_out: bool
    budget_exceeded: str | None
    stdout: str
    stderr: str
    metrics: UsageMetrics


def _terminate_process(proc: subprocess.Popen[bytes]) -> None:
    if proc.poll() is not None:
        return
    try:
        os.killpg(proc.pid, signal.SIGTERM)
        proc.wait(timeout=2)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.wait(timeout=2)


def _run_monitored(
    command: list[str],
    *,
    cwd: Path,
    env: dict[str, str],
    run_dir: Path,
    task: TaskSpec,
    trace: TraceRecorder,
) -> AgentResult:
    event_file = run_dir / "events.jsonl"
    event_file.write_text("", encoding="utf-8")
    stdout_path = run_dir / "agent.stdout.log"
    stderr_path = run_dir / "agent.stderr.log"
    monitor = BudgetMonitor(event_file, task.budgets, trace)
    timed_out = False
    started = time.monotonic()

    with stdout_path.open("wb") as stdout_handle, stderr_path.open("wb") as stderr_handle:
        proc = subprocess.Popen(
            command,
            cwd=cwd,
            env=env,
            stdout=stdout_handle,
            stderr=stderr_handle,
            start_new_session=True,
        )
        while proc.poll() is None:
            exceeded = monitor.poll()
            if exceeded:
                _terminate_process(proc)
                break
            if time.monotonic() - started > task.budgets.wall_time:
                timed_out = True
                trace.emit(
                    "budget.exceeded",
                    data={
                        "budget": "wall_time",
                        "limit": task.budgets.wall_time,
                        "value": time.monotonic() - started,
                    },
                )
                _terminate_process(proc)
                break
            time.sleep(0.1)

        returncode = proc.wait()

    monitor.finish()
    return AgentResult(
        returncode=returncode,
        timed_out=timed_out,
        budget_exceeded=monitor.exceeded,
        stdout=stdout_path.read_text(encoding="utf-8", errors="replace"),
        stderr=stderr_path.read_text(encoding="utf-8", errors="replace"),
        metrics=monitor.metrics,
    )


class CLIAdapter:
    """Trusted local-process adapter used for development."""

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
        environment_project: str | None,
        environment_network: str | None,
        seed: int,
        gateway_url: str | None = None,
        gateway_token: str | None = None,
        gateway_network: str | None = None,
    ) -> AgentResult:
        del environment_network, gateway_network
        env = os.environ.copy()
        env.update(self.spec.env)
        env.update(
            {
                "REDHARNESS_TASK_ID": task.id,
                "REDHARNESS_TASK_DIR": str(task_dir),
                "REDHARNESS_RUN_DIR": str(run_dir),
                "REDHARNESS_OBJECTIVE": task.objective.description,
                "REDHARNESS_ENV_PROJECT": environment_project or "",
                "REDHARNESS_EVENT_FILE": str(run_dir / "events.jsonl"),
                "REDHARNESS_SEED": str(seed),
            }
        )
        if gateway_url and gateway_token:
            env.update(
                {
                    "REDHARNESS_GATEWAY_URL": gateway_url,
                    "REDHARNESS_GATEWAY_TOKEN": gateway_token,
                    "OPENAI_BASE_URL": f"{gateway_url}/v1",
                    "OPENAI_API_KEY": gateway_token,
                }
            )

        cwd = Path(self.spec.cwd).resolve() if self.spec.cwd else task_dir
        self.trace.emit(
            "agent.started",
            actor="agent",
            data={
                "agent_id": self.spec.id,
                "type": self.spec.type,
                "gateway_injected": gateway_url is not None,
            },
        )
        result = _run_monitored(
            self.spec.command,
            cwd=cwd,
            env=env,
            run_dir=run_dir,
            task=task,
            trace=self.trace,
        )
        self._finish_trace(result)
        return result

    def _finish_trace(self, result: AgentResult) -> None:
        self.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
            },
        )


class DockerAdapter:
    """Containerized agent adapter with restrictive defaults and multi-network support."""

    def __init__(self, spec: AgentSpec, *, trace: TraceRecorder) -> None:
        if shutil.which("docker") is None:
            raise AgentError("docker executable was not found")
        self.spec = spec
        self.trace = trace

    @staticmethod
    def _docker(
        command: list[str],
        *,
        cwd: Path,
        check: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(
            command,
            cwd=cwd,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        if check and result.returncode != 0:
            raise AgentError(f"{' '.join(command[:3])} failed: {result.stderr.strip()}")
        return result

    def _create_command(
        self,
        task: TaskSpec,
        *,
        task_dir: Path,
        run_dir: Path,
        container_name: str,
        network: str,
        environment_project: str | None,
        seed: int,
        gateway_url: str | None,
        gateway_token: str | None,
        host_gateway: bool,
    ) -> list[str]:
        command = [
            "docker",
            "create",
            "--rm",
            "--pull=never",
            "--name",
            container_name,
            "--read-only",
            "--cap-drop",
            "ALL",
            "--security-opt",
            "no-new-privileges",
            "--pids-limit",
            "256",
            "--memory",
            "2g",
            "--cpus",
            "2",
            "--tmpfs",
            "/tmp:rw,nosuid,nodev,size=256m",
            "--network",
            network,
        ]
        if self.spec.runtime:
            command.extend(["--runtime", self.spec.runtime])
        if host_gateway:
            command.extend(["--add-host", "host.docker.internal:host-gateway"])

        command.extend(
            [
                "-v",
                f"{task_dir.resolve()}:/task:ro",
                "-v",
                f"{run_dir.resolve()}:/run/redharness:rw",
                "-w",
                "/task",
                "-e",
                f"REDHARNESS_TASK_ID={task.id}",
                "-e",
                "REDHARNESS_TASK_DIR=/task",
                "-e",
                "REDHARNESS_RUN_DIR=/run/redharness",
                "-e",
                f"REDHARNESS_OBJECTIVE={task.objective.description}",
                "-e",
                f"REDHARNESS_ENV_PROJECT={environment_project or ''}",
                "-e",
                "REDHARNESS_EVENT_FILE=/run/redharness/events.jsonl",
                "-e",
                f"REDHARNESS_SEED={seed}",
            ]
        )
        if gateway_url and gateway_token:
            command.extend(
                [
                    "-e",
                    f"REDHARNESS_GATEWAY_URL={gateway_url}",
                    "-e",
                    f"REDHARNESS_GATEWAY_TOKEN={gateway_token}",
                    "-e",
                    f"OPENAI_BASE_URL={gateway_url}/v1",
                    "-e",
                    f"OPENAI_API_KEY={gateway_token}",
                ]
            )

        for key, value in sorted(self.spec.env.items()):
            command.extend(["-e", f"{key}={value}"])
        command.append(str(self.spec.image))
        command.extend(self.spec.command)
        return command

    def run(
        self,
        task: TaskSpec,
        *,
        task_dir: Path,
        run_dir: Path,
        environment_project: str | None,
        environment_network: str | None,
        seed: int,
        gateway_url: str | None = None,
        gateway_token: str | None = None,
        gateway_network: str | None = None,
    ) -> AgentResult:
        gateway_enabled = bool(gateway_url and gateway_token)
        if gateway_enabled and self.spec.network == "none":
            raise AgentError("Docker Agent network:none is incompatible with per-run Gateway")

        container_name = ("rh_agent_" + run_dir.name.lower()).replace("-", "_")[:63]
        sidecar_gateway = gateway_enabled and gateway_network is not None
        host_gateway = gateway_enabled and not sidecar_gateway

        if sidecar_gateway:
            primary_network = str(gateway_network)
        elif self.spec.network == "environment":
            primary_network = environment_network or ("bridge" if host_gateway else "none")
        else:
            primary_network = "none"

        create_command = self._create_command(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            container_name=container_name,
            network=primary_network,
            environment_project=environment_project,
            seed=seed,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
            host_gateway=host_gateway,
        )

        self.trace.emit(
            "agent.started",
            actor="agent",
            data={
                "agent_id": self.spec.id,
                "type": self.spec.type,
                "image": self.spec.image,
                "network": primary_network,
                "target_network": environment_network,
                "runtime": self.spec.runtime,
                "gateway_injected": gateway_enabled,
                "gateway_network": gateway_network,
            },
        )

        try:
            self._docker(create_command, cwd=task_dir)
            if (
                sidecar_gateway
                and environment_network
                and environment_network != gateway_network
            ):
                self._docker(
                    [
                        "docker",
                        "network",
                        "connect",
                        environment_network,
                        container_name,
                    ],
                    cwd=task_dir,
                )

            result = _run_monitored(
                ["docker", "start", "-a", container_name],
                cwd=task_dir,
                env=os.environ.copy(),
                run_dir=run_dir,
                task=task,
                trace=self.trace,
            )
        finally:
            subprocess.run(
                ["docker", "rm", "-f", container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )

        self.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
            },
        )
        return result


def build_agent_adapter(
    spec: AgentSpec,
    *,
    allow_host_agent: bool,
    trace: TraceRecorder,
):
    if spec.type == "cli":
        return CLIAdapter(spec, allow_host=allow_host_agent, trace=trace)
    if spec.type == "docker":
        return DockerAdapter(spec, trace=trace)
    if spec.type == "pi":
        from .pi_adapter import PiAdapter

        return PiAdapter(spec, allow_host=allow_host_agent, trace=trace)
    raise AgentError(f"unsupported agent adapter: {spec.type}")
