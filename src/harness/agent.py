from __future__ import annotations

import os
import shutil
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path

from .agent_workspace import (
    prepare_agent_task_view,
    prepare_agent_workspace_for_container,
)
from .budget import BudgetMonitor, UsageMetrics
from .models import AgentSpec, TaskSpec, _is_reserved_credential_env
from .secureio import open_regular_file, read_regular_text
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
    cancel_path: Path | None = None,
    guard_parent_death: bool = False,
) -> AgentResult:
    event_file = run_dir / "events.jsonl"
    event_file.write_text("", encoding="utf-8")
    stdout_path = run_dir / "agent.stdout.log"
    stderr_path = run_dir / "agent.stderr.log"
    monitor = BudgetMonitor(event_file, task.budgets, trace)
    timed_out = False
    started = time.monotonic()

    with (
        open_regular_file(stdout_path, "wb") as stdout_handle,
        open_regular_file(stderr_path, "wb") as stderr_handle,
    ):
        launch_command = command
        if guard_parent_death and sys.platform == "linux":
            guard = Path(__file__).with_name("process_guard.py")
            launch_command = [sys.executable, str(guard), "--", *command]
        proc = subprocess.Popen(
            launch_command,
            cwd=cwd,
            env=env,
            stdout=stdout_handle,
            stderr=stderr_handle,
            start_new_session=True,
        )
        while proc.poll() is None:
            if cancel_path is not None:
                try:
                    cancelled = bool(read_regular_text(cancel_path, max_bytes=64 * 1024))
                except FileNotFoundError:
                    cancelled = False
                except OSError:
                    cancelled = True
                if cancelled:
                    trace.emit("agent.cancelled", data={"reason": "session_closed"})
                    _terminate_process(proc)
                    break
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
        stdout=read_regular_text(stdout_path),
        stderr=read_regular_text(stderr_path),
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
        agent_task_dir: Path | None = None,
        cancel_path: Path | None = None,
    ) -> AgentResult:
        del agent_task_dir  # Host Agents remain an explicit, unsandboxed development path.
        from .network_policy import enforced_network

        enforced_network(
            self.spec,
            environment_network=environment_network,
            gateway_network=gateway_network,
            gateway_enabled=bool(gateway_url and gateway_token),
        )
        del environment_network, gateway_network
        env = os.environ.copy()
        env.update(self.spec.env)
        env.update(
            {
                "HARNESS_TASK_ID": task.id,
                "HARNESS_TASK_DIR": str(task_dir),
                "HARNESS_RUN_DIR": str(run_dir),
                "HARNESS_OBJECTIVE": task.objective.description,
                "HARNESS_ENV_PROJECT": environment_project or "",
                "HARNESS_EVENT_FILE": str(run_dir / "events.jsonl"),
                "HARNESS_WORLD_INBOX": str(run_dir / "world.inbox.jsonl"),
                "HARNESS_WORLD_CONTEXT": str(run_dir / "world.context.txt"),
                "HARNESS_SUBMISSION_INBOX": str(run_dir / "submission.inbox.jsonl"),
                "HARNESS_FEEDBACK_FILE": str(run_dir / "agent.feedback.jsonl"),
                "HARNESS_PROGRESS_FILE": str(run_dir / "progress.json"),
                "HARNESS_NETWORK_PROFILE": self.spec.network_profile,
                "HARNESS_SEED": str(seed),
            }
        )
        if gateway_url and gateway_token:
            env.update(
                {
                    "HARNESS_GATEWAY_URL": gateway_url,
                    "HARNESS_GATEWAY_TOKEN": gateway_token,
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
            cancel_path=cancel_path,
            guard_parent_death=True,
        )
        self._finish_trace(result)
        return result

    async def start_session(
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
        agent_task_dir: Path | None = None,
    ):
        del agent_task_dir
        from .session import OneShotAgentSession

        return await OneShotAgentSession.start(
            self,
            run_dir=run_dir,
            run_kwargs={
                "task": task,
                "task_dir": task_dir,
                "run_dir": run_dir,
                "environment_project": environment_project,
                "environment_network": environment_network,
                "seed": seed,
                "gateway_url": gateway_url,
                "gateway_token": gateway_token,
                "gateway_network": gateway_network,
                "cancel_path": run_dir / "agent.control.jsonl",
            },
        )

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
        agent_task_dir: Path | None = None,
    ) -> list[str]:
        from .network_policy import proxy_environment_overrides, reject_agent_proxy_env

        forbidden_env = sorted(
            key for key in self.spec.env if _is_reserved_credential_env(key)
        )
        if forbidden_env:
            raise AgentError("container Agent cannot receive reserved credential environment variables")
        reject_agent_proxy_env(self.spec.network_profile, set(self.spec.env))
        agent_task_dir = agent_task_dir or prepare_agent_task_view(
            task_dir=task_dir,
            run_root=run_dir.parent,
            verifier_entrypoint=task.verification.entrypoint,
        )
        container_uid, container_gid = prepare_agent_workspace_for_container(run_dir)
        command = [
            "docker",
            "create",
            "--rm",
            "--pull=never",
            "--name",
            container_name,
            "--user",
            f"{container_uid}:{container_gid}",
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
                f"{agent_task_dir.resolve()}:/task:ro",
                "-v",
                f"{run_dir.resolve()}:/run/harness:rw",
                "-w",
                "/task",
                "-e",
                f"HARNESS_TASK_ID={task.id}",
                "-e",
                "HARNESS_TASK_DIR=/task",
                "-e",
                "HARNESS_RUN_DIR=/run/harness",
                "-e",
                f"HARNESS_OBJECTIVE={task.objective.description}",
                "-e",
                f"HARNESS_ENV_PROJECT={environment_project or ''}",
                "-e",
                "HARNESS_EVENT_FILE=/run/harness/events.jsonl",
                "-e",
                "HARNESS_WORLD_INBOX=/run/harness/world.inbox.jsonl",
                "-e",
                "HARNESS_WORLD_CONTEXT=/run/harness/world.context.txt",
                "-e",
                "HARNESS_SUBMISSION_INBOX=/run/harness/submission.inbox.jsonl",
                "-e",
                "HARNESS_FEEDBACK_FILE=/run/harness/agent.feedback.jsonl",
                "-e",
                "HARNESS_PROGRESS_FILE=/run/harness/progress.json",
                "-e",
                f"HARNESS_NETWORK_PROFILE={self.spec.network_profile}",
                "-e",
                f"HARNESS_SEED={seed}",
            ]
        )
        if gateway_url and gateway_token:
            command.extend(
                [
                    "-e",
                    f"HARNESS_GATEWAY_URL={gateway_url}",
                    "-e",
                    f"HARNESS_GATEWAY_TOKEN={gateway_token}",
                    "-e",
                    f"OPENAI_BASE_URL={gateway_url}/v1",
                    "-e",
                    f"OPENAI_API_KEY={gateway_token}",
                ]
            )

        for key, value in sorted(self.spec.env.items()):
            command.extend(["-e", f"{key}={value}"])
        command.extend(proxy_environment_overrides(self.spec.network_profile))
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
        agent_task_dir: Path | None = None,
        cancel_path: Path | None = None,
    ) -> AgentResult:
        from .network_policy import enforced_network

        gateway_enabled = bool(gateway_url and gateway_token)
        enforced = enforced_network(
            self.spec,
            environment_network=environment_network,
            gateway_network=gateway_network,
            gateway_enabled=gateway_enabled,
        )
        if gateway_enabled and (
            self.spec.network == "none" or self.spec.network_profile == "offline"
        ):
            raise AgentError("Docker Agent offline networking is incompatible with per-run Gateway")

        container_name = ("rh_agent_" + run_dir.name.lower()).replace("-", "_")[:63]
        sidecar_gateway = gateway_enabled and gateway_network is not None
        host_gateway = gateway_enabled and not sidecar_gateway and self.spec.network != "host"

        if enforced is not None:
            primary_network = enforced.primary
        elif self.spec.network_profile == "offline":
            primary_network = "none"
        elif sidecar_gateway:
            if self.spec.network == "host":
                raise AgentError("Docker Agent network:host is incompatible with sidecar Gateway")
            primary_network = str(gateway_network)
        elif self.spec.network == "host":
            primary_network = "host"
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
            agent_task_dir=agent_task_dir,
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
            enforced_target = enforced.environment if enforced is not None else None
            target_differs = (
                enforced_target is not None and enforced_target != primary_network
                if enforced is not None
                else bool(environment_network and environment_network != gateway_network)
            )
            if sidecar_gateway and environment_network and target_differs:
                self._docker(
                    [
                        "docker",
                        "network",
                        "connect",
                        enforced_target or environment_network,
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
                cancel_path=cancel_path,
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

    async def start_session(
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
        agent_task_dir: Path | None = None,
    ):
        from .session import OneShotAgentSession

        return await OneShotAgentSession.start(
            self,
            run_dir=run_dir,
            run_kwargs={
                "task": task,
                "task_dir": task_dir,
                "agent_task_dir": agent_task_dir,
                "run_dir": run_dir,
                "environment_project": environment_project,
                "environment_network": environment_network,
                "seed": seed,
                "gateway_url": gateway_url,
                "gateway_token": gateway_token,
                "gateway_network": gateway_network,
                "cancel_path": run_dir / "agent.control.jsonl",
            },
        )


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
        from .pi_container import ContainerPiAdapter

        return ContainerPiAdapter(spec, allow_host=allow_host_agent, trace=trace)
    raise AgentError(f"unsupported agent adapter: {spec.type}")
