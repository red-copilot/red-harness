from __future__ import annotations

import asyncio
import json
import os
import shutil
import subprocess
import time
import uuid
from collections.abc import AsyncIterator
from pathlib import Path
from typing import Any

from .agent import AgentError, AgentResult, _terminate_process
from .budget import BudgetMonitor, UsageMetrics
from .models import AgentSpec, TaskSpec
from .pi_adapter import PiAdapter
from .session import AgentCheckpoint, AgentEvent, AgentObservation, OneShotAgentSession
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


class ContainerPiRpcSession:
    """Interactive Pi RPC session with Harness-controlled stdin steering."""

    _STEER_TYPES = frozenset(
        {
            "solver.verification",
            "solver.plan.updated",
            "solver.replan_requested",
            "aci.feedback",
            "benchmark.feedback",
            "world.state.updated",
        }
    )

    def __init__(self, adapter: ContainerPiAdapter, *, run_kwargs: dict[str, Any]) -> None:
        self.adapter = adapter
        self.run_kwargs = run_kwargs
        self.run_dir = Path(run_kwargs["run_dir"])
        self.container_name = ("harness_pi_" + self.run_dir.name.lower()).replace("-", "_")[:63]
        self.event_path = self.run_dir / "events.jsonl"
        self.feedback_path = self.run_dir / "agent.feedback.jsonl"
        self.control_path = self.run_dir / "agent.control.jsonl"
        self.raw_path = self.run_dir / "agent.stdout.log"
        self.stderr_path = self.run_dir / "agent.stderr.log"
        self.process: asyncio.subprocess.Process | None = None
        self._stderr_handle = None
        self._write_lock = asyncio.Lock()
        self._pending_observations: list[AgentObservation] = []
        self._prompt_sent = False
        self._events_consumed = False
        self._closed = False
        self._settled = False
        self._timed_out = False
        self._budget_exceeded: str | None = None
        self._event_offset = 0
        self._feedback_offset = 0
        self._rpc_line = 0
        self._started = 0.0
        self._metrics = UsageMetrics()
        self._task = run_kwargs["task"]
        self._gateway_enabled = bool(
            run_kwargs.get("gateway_url") and run_kwargs.get("gateway_token")
        )

    @classmethod
    async def start(
        cls,
        adapter: ContainerPiAdapter,
        *,
        run_kwargs: dict[str, Any],
    ) -> ContainerPiRpcSession:
        session = cls(adapter, run_kwargs=run_kwargs)
        try:
            await session._start()
        except Exception:
            if session._stderr_handle is not None:
                session._stderr_handle.close()
                session._stderr_handle = None
            await asyncio.to_thread(
                subprocess.run,
                ["docker", "rm", "-f", session.container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            raise
        return session

    async def _start(self) -> None:
        task = self.run_kwargs["task"]
        task_dir = Path(self.run_kwargs["task_dir"])
        run_dir = self.run_dir
        environment_network = self.run_kwargs.get("environment_network")
        gateway_url = self.run_kwargs.get("gateway_url")
        gateway_token = self.run_kwargs.get("gateway_token")
        gateway_network = self.run_kwargs.get("gateway_network")
        gateway_enabled = bool(gateway_url and gateway_token)
        if gateway_enabled and (
            self.adapter.spec.network == "none" or self.adapter.spec.network_profile == "offline"
        ):
            raise AgentError("Pi offline networking is incompatible with per-run Gateway")

        sidecar_gateway = gateway_enabled and gateway_network is not None
        if self.adapter.spec.network_profile == "offline":
            primary_network = "none"
        elif sidecar_gateway:
            if self.adapter.spec.network == "host":
                raise AgentError("Pi network:host is incompatible with sidecar Gateway mode")
            primary_network = str(gateway_network)
        elif self.adapter.spec.network == "host":
            primary_network = "host"
            if gateway_enabled:
                gateway_url = gateway_url.replace("host.docker.internal", "127.0.0.1")
        elif self.adapter.spec.network == "environment":
            primary_network = environment_network or "bridge"
        else:
            primary_network = "none"

        self.event_path.write_text("", encoding="utf-8")
        self.raw_path.write_text("", encoding="utf-8")
        self.stderr_path.write_text("", encoding="utf-8")
        self.feedback_path.touch(exist_ok=True)
        self.control_path.touch(exist_ok=True)
        (run_dir / "pi-sessions").mkdir(parents=True, exist_ok=True)
        (run_dir / "home").mkdir(parents=True, exist_ok=True)
        self.adapter._prepare_agent_dir(
            run_dir=run_dir,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
        )
        host_gateway = gateway_enabled and not sidecar_gateway and primary_network != "host"
        create_command = self.adapter._container_command(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            container_name=self.container_name,
            network=primary_network,
            environment_project=self.run_kwargs.get("environment_project"),
            seed=self.run_kwargs["seed"],
            gateway_url=gateway_url,
            gateway_token=gateway_token,
            host_gateway=host_gateway,
        )
        self.adapter.trace.emit(
            "agent.started",
            actor="agent",
            data={
                "agent_id": self.adapter.spec.id,
                "type": "pi",
                "execution": "docker",
                "image": self.adapter.spec.image,
                "network": primary_network,
                "target_network": environment_network,
                "runtime": self.adapter.spec.runtime,
                "model": self.adapter.pi.model,
                "provider": "harness" if gateway_enabled else self.adapter.pi.provider,
                "thinking": self.adapter.pi.thinking,
                "tools": self.adapter.pi.tools,
                "pi_protocol": "rpc",
            },
        )
        await asyncio.to_thread(self.adapter._docker, create_command, cwd=task_dir)
        if sidecar_gateway and environment_network and environment_network != gateway_network:
            await asyncio.to_thread(
                self.adapter._docker,
                ["docker", "network", "connect", str(environment_network), self.container_name],
                cwd=task_dir,
            )
        self._stderr_handle = self.stderr_path.open("ab")
        self.process = await asyncio.create_subprocess_exec(
            "docker",
            "start",
            "-ai",
            self.container_name,
            cwd=task_dir,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=self._stderr_handle,
            start_new_session=True,
            limit=64 * 1024 * 1024,
        )
        self._started = time.monotonic()

    async def events(self) -> AsyncIterator[AgentEvent]:
        if self.process is None:
            raise RuntimeError("session has not been started")
        if self._events_consumed:
            raise RuntimeError("Pi RPC event stream can only be consumed once")
        self._events_consumed = True
        task = self._task
        initial_prompt = self.adapter._prompt_text(task)
        if self._pending_observations:
            initial_prompt += "\n\nHarness state before the first model turn:\n" + "\n".join(
                self._format_observation(item) for item in self._pending_observations
            )
            self._pending_observations.clear()
        self._prompt_sent = True
        await self._send_command(
            {
                "id": f"harness-steering-mode-{uuid.uuid4().hex}",
                "type": "set_steering_mode",
                "mode": "all",
            }
        )
        await self._send_command(
            {
                "id": f"harness-prompt-{uuid.uuid4().hex}",
                "type": "prompt",
                "message": initial_prompt,
            }
        )
        assert self.process.stdout is not None
        while self.process.returncode is None:
            remaining = task.budgets.wall_time - (time.monotonic() - self._started)
            if remaining <= 0:
                self._timed_out = True
                self.adapter.trace.emit(
                    "budget.exceeded",
                    data={"budget": "wall_time", "limit": task.budgets.wall_time},
                )
                await self._kill_container()
                break
            try:
                line = await asyncio.wait_for(self.process.stdout.readline(), timeout=remaining)
            except TimeoutError:
                self._timed_out = True
                self.adapter.trace.emit(
                    "budget.exceeded",
                    data={"budget": "wall_time", "limit": task.budgets.wall_time},
                )
                await self._kill_container()
                break
            if not line:
                break
            self._rpc_line += 1
            raw = line.decode("utf-8", errors="replace")
            with self.raw_path.open("a", encoding="utf-8") as output:
                output.write(raw)
            try:
                record = json.loads(raw)
            except json.JSONDecodeError:
                record = None
            if isinstance(record, dict) and record.get("type") == "response":
                if record.get("success") is False:
                    event = AgentEvent(
                        type="agent.telemetry_error",
                        data={
                            "source": "pi.rpc",
                            "command_id": record.get("id"),
                            "message": str(record.get("error", "RPC command failed")),
                        },
                        event_id=f"pi-rpc:{self._rpc_line}:0",
                    )
                    self._persist_event(event)
                    yield event
                continue
            normalized = self.adapter._normalize_line(
                raw.rstrip("\r\n"),
                account_model=not self._gateway_enabled,
            )
            for index, (event_type, data) in enumerate(normalized):
                event = AgentEvent(
                    type=event_type,
                    data=data,
                    event_id=f"pi-rpc:{self._rpc_line}:{index}",
                )
                self._persist_event(event)
                self._account(event)
                self.adapter.trace.emit(event.type, actor="agent", data=event.data)
                if event.type == "pi.agent_settled":
                    self._settled = True
                yield event
            self._check_budget()
            if self._budget_exceeded:
                await self._kill_container()
                break
            if self._settled:
                break

        if not self._settled and not self._timed_out and not self._budget_exceeded:
            event = AgentEvent(
                type="agent.session.crashed",
                data={"returncode": self.process.returncode},
                event_id=f"pi-rpc:{self._rpc_line}:crash",
            )
            self._persist_event(event)
            yield event

    async def observe(self, observation: AgentObservation) -> None:
        record = {
            "id": f"feedback_{uuid.uuid4().hex}",
            "type": observation.type,
            "data": observation.data,
        }
        line = json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n"
        await asyncio.to_thread(self._append_feedback, line)
        if not self._prompt_sent:
            self._pending_observations.append(observation)
        elif observation.type in self._STEER_TYPES and not self._closed:
            try:
                settled = self._settled
                await self._send_command(
                    {
                        "id": f"harness-steer-{uuid.uuid4().hex}",
                        "type": "prompt" if settled else "steer",
                        "message": self._format_observation(observation),
                    }
                )
                if settled:
                    # A settled RPC agent is idle between turns. Prompt starts
                    # the next turn; steer is reserved for an active turn.
                    self._settled = False
            except (BrokenPipeError, ConnectionResetError, RuntimeError) as exc:
                self.adapter.trace.emit(
                    "agent.rpc.steer_failed",
                    actor="harness",
                    data={"observation_type": observation.type, "error_type": type(exc).__name__},
                )

    async def checkpoint(self) -> AgentCheckpoint:
        return AgentCheckpoint(
            id=f"pi-rpc-{self.container_name}-{self._rpc_line}",
            event_offset=self._event_offset,
            feedback_offset=self._feedback_offset,
        )

    async def close(self, reason: str) -> None:
        if self._closed:
            return
        self._closed = True
        record = {"type": "session.close_requested", "data": {"reason": reason}}
        line = json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n"
        await asyncio.to_thread(self._append_control, line)
        await self._kill_container()

    async def result(self) -> AgentResult:
        if self.process is None:
            raise RuntimeError("session has not been started")
        if self.process.returncode is None and not self._closed:
            await self._kill_container()
            self._closed = True
        if self.process.stdin is not None and not self.process.stdin.is_closing():
            self.process.stdin.close()
        drain_task = asyncio.create_task(self._drain_stdout())
        try:
            returncode = await asyncio.wait_for(self.process.wait(), timeout=15)
        except TimeoutError:
            await self._kill_container()
            try:
                returncode = await asyncio.wait_for(self.process.wait(), timeout=5)
            except TimeoutError:
                returncode = -1
        await drain_task
        if self._stderr_handle is not None:
            self._stderr_handle.close()
            self._stderr_handle = None
        await asyncio.to_thread(
            subprocess.run,
            ["docker", "rm", "-f", self.container_name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        result = AgentResult(
            returncode=returncode,
            timed_out=self._timed_out,
            budget_exceeded=self._budget_exceeded,
            stdout=self.raw_path.read_text(encoding="utf-8", errors="replace"),
            stderr=self.stderr_path.read_text(encoding="utf-8", errors="replace"),
            metrics=self._metrics,
        )
        self.adapter.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
                "pi_protocol": "rpc",
                "containerized": True,
            },
        )
        return result

    def _persist_event(self, event: AgentEvent) -> None:
        payload = {"type": event.type, "data": event.data, "event_id": event.event_id}
        encoded = json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n"
        with self.event_path.open("a", encoding="utf-8") as handle:
            handle.write(encoded)
            handle.flush()
            self._event_offset = handle.tell()

    def _append_feedback(self, line: str) -> None:
        with self.feedback_path.open("a", encoding="utf-8") as handle:
            handle.write(line)
            handle.flush()
            self._feedback_offset = handle.tell()

    def _append_control(self, line: str) -> None:
        with self.control_path.open("a", encoding="utf-8") as handle:
            handle.write(line)
            handle.flush()

    async def _send_command(self, command: dict[str, Any]) -> None:
        if self.process is None or self.process.stdin is None or self.process.stdin.is_closing():
            return
        line = json.dumps(command, ensure_ascii=False, separators=(",", ":")) + "\n"
        async with self._write_lock:
            self.process.stdin.write(line.encode("utf-8"))
            await self.process.stdin.drain()

    @staticmethod
    def _format_observation(observation: AgentObservation) -> str:
        payload = json.dumps(observation.data, ensure_ascii=False, sort_keys=True)
        return f"Harness feedback ({observation.type}): {payload}"

    def _account(self, event: AgentEvent) -> None:
        if event.type == "model.request":
            self._metrics.model_calls += 1
        elif event.type == "model.usage":
            self._metrics.input_tokens += int(event.data.get("input_tokens", 0) or 0)
            self._metrics.output_tokens += int(event.data.get("output_tokens", 0) or 0)
            self._metrics.total_tokens += int(event.data.get("total_tokens", 0) or 0)
            self._metrics.cost_usd += float(event.data.get("cost_usd", 0.0) or 0.0)
        elif event.type == "tool.call":
            self._metrics.tool_calls += 1

    def _check_budget(self) -> None:
        budgets = self._task.budgets
        checks = (
            ("max_tokens", budgets.max_tokens, self._metrics.total_tokens),
            ("max_model_calls", budgets.max_model_calls, self._metrics.model_calls),
            ("max_tool_calls", budgets.max_tool_calls, self._metrics.tool_calls),
            ("max_cost_usd", budgets.max_cost_usd, self._metrics.cost_usd),
        )
        for name, limit, used in checks:
            if limit is not None and used > limit:
                self._budget_exceeded = name
                self.adapter.trace.emit(
                    "budget.exceeded",
                    data={"budget": name, "limit": limit, "value": used},
                )
                return

    async def _kill_container(self) -> None:
        await asyncio.to_thread(
            subprocess.run,
            ["docker", "kill", self.container_name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )

    async def _drain_stdout(self) -> None:
        if self.process is None or self.process.stdout is None:
            return
        while line := await self.process.stdout.readline():
            with self.raw_path.open("ab") as output:
                output.write(line)


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

        if self.pi.mode == "rpc":
            command.append("-i")
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
    ) -> ContainerPiSession | ContainerPiRpcSession:
        run_kwargs = {
            "task": task,
            "task_dir": task_dir,
            "run_dir": run_dir,
            "environment_project": environment_project,
            "environment_network": environment_network,
            "seed": seed,
            "gateway_url": gateway_url,
            "gateway_token": gateway_token,
            "gateway_network": gateway_network,
        }
        if self.pi.mode == "rpc":
            return await ContainerPiRpcSession.start(self, run_kwargs=run_kwargs)
        return await ContainerPiSession.start(self, run_dir=run_dir, run_kwargs=run_kwargs)

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
        if self.pi.mode == "rpc":
            raise AgentError(
                "Pi RPC mode requires start_session(); use mode: json for one-shot runs"
            )
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
            if sidecar_gateway and environment_network and environment_network != gateway_network:
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
