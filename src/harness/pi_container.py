# ruff: noqa: I001
from __future__ import annotations

import asyncio
import os
import shutil
import subprocess
import time
from pathlib import Path

from .agent import AgentError, AgentResult, _terminate_process
from .budget import BudgetMonitor
from .models import AgentSpec, TaskSpec
from .pi_adapter import PiAdapter
from .session import OneShotAgentSession
from .trace import TraceRecorder




class ContainerPiSession(OneShotAgentSession):
    """Session wrapper that can actively terminate the running Pi container."""

    def __init__(self, adapter, *, run_kwargs, run_dir, poll_interval=0.05) -> None:
        super().__init__(
            adapter,
            run_kwargs=run_kwargs,
            run_dir=run_dir,
            poll_interval=poll_interval,
        )
        self.container_name = ("harness_pi_" + run_dir.name.lower()).replace("-", "_")[:63]

    async def close(self, reason: str) -> None:
        await super().close(reason)
        await asyncio.to_thread(
            subprocess.run,
            ["docker", "kill", self.container_name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )

class ContainerPiAdapter(PiAdapter):
    """Run Pi inside a restricted Docker container while reusing Pi JSON parsing."""

    def __init__(self, spec: AgentSpec, *, allow_host: bool, trace: TraceRecorder) -> None:
        del allow_host
        if shutil.which("docker") is None:
            raise AgentError("docker executable was not found")
        if spec.pi is None:
            raise AgentError("Pi agent requires a pi configuration block")
        if not spec.image:
            raise AgentError("Pi agent requires a container image")
        self.spec = spec
        self.pi = spec.pi
        self.trace = trace
        self._tool_started: dict[str, float] = {}

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

    def _container_command(
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
            "512",
            "--memory",
            "4g",
            "--cpus",
            "4",
            "--tmpfs",
            "/tmp:rw,nosuid,nodev,size=512m",
            "--tmpfs",
            "/var/tmp:rw,nosuid,nodev,size=256m",
            "--network",
            network,
        ]
        if self.spec.runtime:
            command.extend(["--runtime", self.spec.runtime])
        for capability in self.pi.cap_add:
            command.extend(["--cap-add", capability])
        if host_gateway:
            command.extend(["--add-host", "host.docker.internal:host-gateway"])

        command.extend(
            [
                "-v",
                f"{task_dir.resolve()}:/task:ro",
                "-v",
                f"{run_dir.resolve()}:/run/harness:rw",
                "-w",
                "/run/harness",
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
                "-e",
                "PI_CODING_AGENT_DIR=/run/harness/pi-agent",
                "-e",
                "PI_CODING_AGENT_SESSION_DIR=/run/harness/pi-sessions",
                "-e",
                "PI_TELEMETRY=0",
                "-e",
                "PI_SKIP_VERSION_CHECK=1",
                "-e",
                "HOME=/run/harness/home",
            ]
        )
        if self.pi.offline or self.spec.network_profile != "unrestricted":
            command.extend(["-e", "PI_OFFLINE=1"])
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
        if not (gateway_url and gateway_token):
            for key in self.pi.env_passthrough:
                if key not in os.environ:
                    raise AgentError(f"requested Pi environment variable is missing: {key}")
                command.extend(["-e", f"{key}={os.environ[key]}"])

        command.append(str(self.spec.image))
        command.extend(self._command(task, gateway_enabled=bool(gateway_url and gateway_token)))
        return command

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
    ) -> ContainerPiSession:
        return await ContainerPiSession.start(
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
            },
        )

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
        if gateway_enabled and (
            self.spec.network == "none" or self.spec.network_profile == "offline"
        ):
            raise AgentError("Pi offline networking is incompatible with per-run Gateway")

        if self.spec.network == "host" and gateway_url and gateway_network is None:
            gateway_url = gateway_url.replace("host.docker.internal", "127.0.0.1")

        event_file = run_dir / "events.jsonl"
        event_file.write_text("", encoding="utf-8")
        raw_path = run_dir / "agent.stdout.log"
        stderr_path = run_dir / "agent.stderr.log"
        raw_path.write_text("", encoding="utf-8")
        self._prepare_agent_dir(
            run_dir=run_dir,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
        )
        (run_dir / "pi-sessions").mkdir(parents=True, exist_ok=True)
        (run_dir / "home").mkdir(parents=True, exist_ok=True)

        container_name = ("harness_pi_" + run_dir.name.lower()).replace("-", "_")[:63]
        sidecar_gateway = gateway_enabled and gateway_network is not None
        host_gateway = gateway_enabled and not sidecar_gateway and self.spec.network != "host"
        if self.spec.network_profile == "offline":
            primary_network = "none"
        elif sidecar_gateway:
            if self.spec.network == "host":
                raise AgentError("Pi network:host is incompatible with sidecar Gateway mode")
            primary_network = str(gateway_network)
        elif self.spec.network == "host":
            primary_network = "host"
        elif self.spec.network == "environment":
            primary_network = environment_network or "bridge"
        else:
            primary_network = "none"

        create_command = self._container_command(
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
                "type": "pi",
                "execution": "docker",
                "image": self.spec.image,
                "network": primary_network,
                "target_network": environment_network,
                "runtime": self.spec.runtime,
                "cap_add": self.pi.cap_add,
                "model": self.pi.model,
                "provider": "harness" if gateway_enabled else self.pi.provider,
                "thinking": self.pi.thinking,
                "tools": self.pi.tools,
                "gateway_injected": gateway_enabled,
                "gateway_network": gateway_network,
                "network_profile": self.spec.network_profile,
                "network_profile_enforcement": (
                    "docker-none"
                    if self.spec.network_profile == "offline"
                    else "advisory"
                    if self.spec.network_profile == "benchmark-only"
                    else "unrestricted"
                ),
            },
        )

        monitor = BudgetMonitor(event_file, task.budgets, self.trace)
        timed_out = False
        started = time.monotonic()
        offset = 0
        remainder = ""

        try:
            self._docker(create_command, cwd=task_dir)
            if (
                sidecar_gateway
                and environment_network
                and environment_network != gateway_network
            ):
                self._docker(
                    ["docker", "network", "connect", environment_network, container_name],
                    cwd=task_dir,
                )

            with raw_path.open("ab") as stdout_handle, stderr_path.open("wb") as stderr_handle:
                proc = subprocess.Popen(
                    ["docker", "start", "-a", container_name],
                    cwd=task_dir,
                    env=os.environ.copy(),
                    stdout=stdout_handle,
                    stderr=stderr_handle,
                    start_new_session=True,
                )
                while proc.poll() is None:
                    offset, remainder = self._poll_output(
                        raw_path,
                        offset=offset,
                        remainder=remainder,
                        event_file=event_file,
                        account_model=not gateway_enabled,
                    )
                    exceeded = monitor.poll()
                    if exceeded:
                        _terminate_process(proc)
                        break
                    if time.monotonic() - started > task.budgets.wall_time:
                        timed_out = True
                        self.trace.emit(
                            "budget.exceeded",
                            data={
                                "budget": "wall_time",
                                "limit": task.budgets.wall_time,
                                "value": time.monotonic() - started,
                            },
                        )
                        _terminate_process(proc)
                        break
                    time.sleep(0.05)
                returncode = proc.wait()
        finally:
            subprocess.run(
                ["docker", "rm", "-f", container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )

        offset, remainder = self._poll_output(
            raw_path,
            offset=offset,
            remainder=remainder,
            event_file=event_file,
            account_model=not gateway_enabled,
        )
        del offset
        if remainder.strip():
            self._translate_line(
                remainder.strip(),
                event_file=event_file,
                account_model=not gateway_enabled,
            )
        monitor.finish()

        result = AgentResult(
            returncode=returncode,
            timed_out=timed_out,
            budget_exceeded=monitor.exceeded,
            stdout=raw_path.read_text(encoding="utf-8", errors="replace"),
            stderr=stderr_path.read_text(encoding="utf-8", errors="replace"),
            metrics=monitor.metrics,
        )
        self.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
                "pi_protocol": "json",
                "execution": "docker",
                "image": self.spec.image,
            },
        )
        return result
