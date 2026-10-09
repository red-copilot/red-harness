from __future__ import annotations

import asyncio
import hashlib
import json
import math
import os
import re
import shutil
import subprocess
import time
import uuid
from collections.abc import AsyncIterator
from pathlib import Path
from typing import Any

from .agent import AgentError, AgentResult, _terminate_process
from .agent_workspace import (
    prepare_agent_task_view,
    prepare_agent_workspace_for_container,
)
from .budget import BudgetMonitor, UsageMetrics
from .models import AgentSpec, TaskSpec, _is_reserved_credential_env
from .pi_adapter import PiAdapter
from .secureio import open_regular_file, read_regular_text
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
        gateway_usage_path = run_kwargs.get("gateway_usage_path")
        self.gateway_usage_path = Path(gateway_usage_path) if gateway_usage_path else None
        self._gateway_event_offset = int(run_kwargs.get("resume_gateway_event_offset", 0))
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
        self._rpc_epoch = uuid.uuid4().hex
        self._startup_records: list[bytes] = []
        self._pi_session_id: str | None = None
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
        from .network_policy import enforced_network

        task = self.run_kwargs["task"]
        task_dir = Path(self.run_kwargs["task_dir"])
        agent_task_dir = self.run_kwargs.get("agent_task_dir")
        if agent_task_dir is None:
            agent_task_dir = prepare_agent_task_view(
                task_dir=task_dir,
                run_root=self.run_dir.parent,
                verifier_entrypoint=task.verification.entrypoint,
            )
        run_dir = self.run_dir
        if self.run_kwargs.get("resume_session_id"):
            await self._guard_existing_resume_container(task_dir)
        environment_network = self.run_kwargs.get("environment_network")
        gateway_url = self.run_kwargs.get("gateway_url")
        gateway_token = self.run_kwargs.get("gateway_token")
        gateway_network = self.run_kwargs.get("gateway_network")
        gateway_enabled = bool(gateway_url and gateway_token)
        enforced = enforced_network(
            self.adapter.spec,
            environment_network=environment_network,
            gateway_network=gateway_network,
            gateway_enabled=gateway_enabled,
        )
        if gateway_enabled and (
            self.adapter.spec.network == "none" or self.adapter.spec.network_profile == "offline"
        ):
            raise AgentError("Pi offline networking is incompatible with per-run Gateway")

        sidecar_gateway = gateway_enabled and gateway_network is not None
        if enforced is not None:
            primary_network = enforced.primary
        elif self.adapter.spec.network_profile == "offline":
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
            agent_task_dir=agent_task_dir,
            run_dir=run_dir,
            container_name=self.container_name,
            network=primary_network,
            environment_project=self.run_kwargs.get("environment_project"),
            seed=self.run_kwargs["seed"],
            gateway_url=gateway_url,
            gateway_token=gateway_token,
            host_gateway=host_gateway,
            agent_session_id=self.run_kwargs.get("resume_session_id"),
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
        enforced_target = enforced.environment if enforced is not None else None
        target_differs = (
            enforced_target is not None and enforced_target != primary_network
            if enforced is not None
            else bool(environment_network and environment_network != gateway_network)
        )
        if sidecar_gateway and environment_network and target_differs:
            await asyncio.to_thread(
                self.adapter._docker,
                [
                    "docker",
                    "network",
                    "connect",
                    str(enforced_target or environment_network),
                    self.container_name,
                ],
                cwd=task_dir,
            )
        self._stderr_handle = open_regular_file(self.stderr_path, "ab")
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
        await self._wait_for_session_identity()

    async def _wait_for_session_identity(self) -> None:
        if self.process is None or self.process.stdout is None:
            raise RuntimeError("Pi RPC process has no stdout")
        try:
            while True:
                line = await asyncio.wait_for(self.process.stdout.readline(), timeout=15)
                if not line:
                    raise AgentError("Pi RPC exited before reporting its session ID")
                self._startup_records.append(line)
                try:
                    record = json.loads(line)
                except (json.JSONDecodeError, UnicodeDecodeError):
                    continue
                if not isinstance(record, dict) or record.get("type") != "session":
                    continue
                session_id = record.get("id")
                if not isinstance(session_id, str) or not re.fullmatch(
                    r"[A-Za-z0-9._-]{1,128}", session_id
                ):
                    raise AgentError("Pi RPC reported an invalid session ID")
                requested_id = self.run_kwargs.get("resume_session_id")
                if requested_id is not None and requested_id != session_id:
                    raise AgentError("Pi RPC did not restore the requested session ID")
                self._pi_session_id = session_id
                return
        except TimeoutError as exc:
            raise AgentError("Pi RPC did not report a session ID during startup") from exc

    async def _guard_existing_resume_container(self, task_dir: Path) -> None:
        inspect = await asyncio.to_thread(
            self.adapter._docker,
            ["docker", "inspect", "--format", "{{.State.Running}}", self.container_name],
            cwd=task_dir,
            check=False,
        )
        if inspect.returncode != 0:
            if "no such object" not in inspect.stderr.lower() and "no such container" not in inspect.stderr.lower():
                raise AgentError(f"cannot inspect prior Pi container: {inspect.stderr.strip()}")
            return
        if inspect.stdout.strip().lower() == "true":
            raise AgentError(
                "prior Pi container is still running; stop it and reconcile any in-flight "
                "action before resuming"
            )
        await asyncio.to_thread(
            self.adapter._docker,
            ["docker", "rm", "-f", self.container_name],
            cwd=task_dir,
            check=False,
        )

    def _normalize_pi_record(self, raw: str) -> list[tuple[str, dict[str, Any]]]:
        """Keep model accounting on the trusted Gateway stream when routed there."""
        return self.adapter._normalize_line(
            raw,
            account_model=not self._gateway_enabled,
        )

    async def events(self) -> AsyncIterator[AgentEvent]:
        if self.process is None:
            raise RuntimeError("session has not been started")
        if self._events_consumed:
            raise RuntimeError("Pi RPC event stream can only be consumed once")
        self._events_consumed = True
        task = self._task
        initial_prompt = self.adapter._prompt_text(task)
        if self.run_kwargs.get("resume_session_id"):
            initial_prompt = (
                "Continue the existing Pi session for this Harness run. First read the current "
                "world.context.txt and progress.json in the project workspace. Treat them as "
                "the current Harness state, avoid repeating completed or interrupted actions, "
                "and continue the objective using only new safe actions.\n\n" + initial_prompt
            )
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
        startup_records = list(self._startup_records)
        self._startup_records.clear()
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
                line = startup_records.pop(0) if startup_records else await asyncio.wait_for(
                    self.process.stdout.readline(), timeout=min(remaining, 0.1)
                )
            except TimeoutError:
                self._consume_gateway_events()
                self._check_budget()
                if self._budget_exceeded:
                    await self._kill_container()
                    break
                if time.monotonic() - self._started >= task.budgets.wall_time:
                    self._timed_out = True
                    self.adapter.trace.emit(
                        "budget.exceeded",
                        data={"budget": "wall_time", "limit": task.budgets.wall_time},
                    )
                    await self._kill_container()
                    break
                continue
            self._consume_gateway_events()
            self._check_budget()
            if self._budget_exceeded:
                await self._kill_container()
                break
            if not line:
                break
            self._rpc_line += 1
            raw = line.decode("utf-8", errors="replace")
            with open_regular_file(self.raw_path, "a") as output:
                output.write(raw)
            try:
                record = json.loads(raw)
            except json.JSONDecodeError:
                record = None
            if isinstance(record, dict) and record.get("type") == "response":
                if record.get("success") is False:
                    command_id = record.get("id")
                    event = AgentEvent(
                        type="agent.telemetry_error",
                        data={
                            "source": "pi.rpc",
                            "command_id_sha256": (
                                hashlib.sha256(command_id.encode("utf-8")).hexdigest()
                                if isinstance(command_id, str)
                                else None
                            ),
                            "error_type": type(record.get("error")).__name__,
                            "message": "Pi RPC command failed",
                        },
                        event_id=f"pi-rpc:{self._rpc_epoch}:{self._rpc_line}:0",
                    )
                    self._persist_event(event)
                    yield event
                continue
            if isinstance(record, dict) and record.get("type") == "session":
                session_id = record.get("id")
                if isinstance(session_id, str) and 0 < len(session_id) <= 256:
                    self._pi_session_id = session_id
            normalized = self._normalize_pi_record(raw.rstrip("\r\n"))
            for index, (event_type, data) in enumerate(normalized):
                event = AgentEvent(
                    type=event_type,
                    data=data,
                    event_id=f"pi-rpc:{self._rpc_epoch}:{self._rpc_line}:{index}",
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
                event_id=f"pi-rpc:{self._rpc_epoch}:{self._rpc_line}:crash",
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
            session_id=self._pi_session_id,
            gateway_event_offset=self._gateway_event_offset,
        )

    @property
    def usage_metrics(self) -> dict[str, int | float]:
        return self._metrics.as_dict()

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
            stdout=read_regular_text(self.raw_path),
            stderr=read_regular_text(self.stderr_path),
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
        with open_regular_file(self.event_path, "a") as handle:
            handle.write(encoded)
            handle.flush()
            self._event_offset = handle.tell()

    def _append_feedback(self, line: str) -> None:
        with open_regular_file(self.feedback_path, "a") as handle:
            handle.write(line)
            handle.flush()
            self._feedback_offset = handle.tell()

    def _append_control(self, line: str) -> None:
        with open_regular_file(self.control_path, "a") as handle:
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
        if event.type == "model.usage_missing":
            self._budget_exceeded = "invalid_telemetry"
            self.adapter.trace.emit(
                "agent.telemetry_error",
                data={"message": "model usage telemetry was missing"},
            )
            self.adapter.trace.emit(
                "budget.exceeded",
                data={
                    "budget": self._budget_exceeded,
                    "reason": "model usage telemetry was missing",
                },
            )
            return
        if event.type == "model.request":
            self._metrics.model_calls += 1
        elif event.type == "model.usage":
            counters = [
                event.data.get(field, 0)
                for field in ("input_tokens", "output_tokens", "total_tokens")
            ]
            cost = event.data.get("cost_usd", 0.0)
            if (
                any(
                    isinstance(value, bool) or not isinstance(value, int) or value < 0
                    for value in counters
                )
                or counters[2] < counters[0] + counters[1]
                or isinstance(cost, bool)
                or not isinstance(cost, (int, float))
                or not math.isfinite(float(cost))
                or cost < 0
            ):
                self._budget_exceeded = "invalid_telemetry"
                self.adapter.trace.emit(
                    "agent.telemetry_error",
                    data={"message": "invalid or negative Pi usage telemetry"},
                )
                self.adapter.trace.emit(
                    "budget.exceeded",
                    data={
                        "budget": self._budget_exceeded,
                        "reason": "untrusted Pi usage telemetry was invalid",
                    },
                )
                return
            self._metrics.input_tokens += counters[0]
            self._metrics.output_tokens += counters[1]
            self._metrics.total_tokens += counters[2]
            self._metrics.cost_usd += float(cost)
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

    def _consume_gateway_events(self) -> None:
        if self.gateway_usage_path is None:
            return
        try:
            with open_regular_file(self.gateway_usage_path, "rb") as handle:
                handle.seek(0, os.SEEK_END)
                if self._gateway_event_offset > handle.tell():
                    self._invalid_gateway_telemetry("gateway usage cursor is ahead of its log")
                    return
                handle.seek(self._gateway_event_offset)
                while True:
                    line = handle.readline(8 * 1024 * 1024 + 1)
                    if not line:
                        return
                    if len(line) > 8 * 1024 * 1024:
                        self._invalid_gateway_telemetry("gateway usage record exceeded size limit")
                        return
                    if not line.endswith(b"\n"):
                        return
                    self._gateway_event_offset = handle.tell()
                    self._consume_gateway_event_line(line)
                    if self._budget_exceeded:
                        return
        except FileNotFoundError:
            if self._gateway_event_offset:
                self._invalid_gateway_telemetry("gateway usage log is missing at the saved cursor")
            return

    def _consume_gateway_event_line(self, line: bytes) -> None:
        try:
            record = json.loads(line)
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._invalid_gateway_telemetry("gateway usage log contains malformed JSON")
            return
        if isinstance(record, dict) and record.get("type") == "model.usage_missing":
            self.adapter.trace.emit("model.usage_missing", actor="gateway")
            self._invalid_gateway_telemetry("gateway did not report valid model usage")
            return
        if not isinstance(record, dict) or record.get("type") not in {
            "model.request",
            "model.usage",
        }:
            return
        data = record.get("data")
        if not isinstance(data, dict):
            self._invalid_gateway_telemetry("gateway usage record has invalid shape")
            return
        event = AgentEvent(
            type=record["type"],
            data=data,
            event_id=f"gateway:{self._gateway_event_offset}",
        )
        self._account(event)
        self.adapter.trace.emit(event.type, actor="gateway", data=event.data)

    def _invalid_gateway_telemetry(self, reason: str) -> None:
        self._budget_exceeded = "invalid_telemetry"
        self.adapter.trace.emit("agent.telemetry_error", data={"message": reason})
        self.adapter.trace.emit(
            "budget.exceeded",
            data={"budget": self._budget_exceeded, "reason": reason},
        )

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
            with open_regular_file(self.raw_path, "ab") as output:
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
        agent_session_id: str | None = None,
        agent_task_dir: Path | None = None,
    ) -> list[str]:
        from .network_policy import proxy_environment_overrides, reject_agent_proxy_env

        env_names = set(self.spec.env) | set(self.pi.env_passthrough)
        if any(_is_reserved_credential_env(key) for key in env_names):
            raise AgentError("container Agent cannot receive reserved credential environment variables")
        reject_agent_proxy_env(self.spec.network_profile, env_names)
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
                f"{agent_task_dir.resolve()}:/task:ro",
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
        command.extend(proxy_environment_overrides(self.spec.network_profile))

        if self.pi.mode == "rpc":
            command.append("-i")
        command.append(str(self.spec.image))
        command.extend(
            self._command(
                task,
                gateway_enabled=bool(gateway_url and gateway_token),
                session_id=agent_session_id,
            )
        )
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
        resume_session_id: str | None = None,
        agent_task_dir: Path | None = None,
    ) -> ContainerPiSession | ContainerPiRpcSession:
        run_kwargs = {
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
            "resume_session_id": resume_session_id,
        }
        if self.pi.mode == "rpc":
            return await ContainerPiRpcSession.start(self, run_kwargs=run_kwargs)
        if resume_session_id is not None:
            raise AgentError("Pi JSON mode cannot restore an Agent session")
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
        agent_task_dir: Path | None = None,
    ) -> AgentResult:
        if self.pi.mode == "rpc":
            raise AgentError(
                "Pi RPC mode requires start_session(); use mode: json for one-shot runs"
            )
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
        if enforced is not None:
            primary_network = enforced.primary
        elif self.spec.network_profile == "offline":
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
            agent_session_id=None,
            agent_task_dir=agent_task_dir,
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
                    if self.spec.network_profile in {"offline", "fully-offline"}
                    else "docker-internal-network"
                    if self.spec.network_profile == "target-only"
                    else "docker-internal-network-gateway-proxy"
                    if self.spec.network_profile == "model-allowed"
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

            with (
                open_regular_file(raw_path, "ab") as stdout_handle,
                open_regular_file(stderr_path, "wb") as stderr_handle,
            ):
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
            stdout=read_regular_text(raw_path),
            stderr=read_regular_text(stderr_path),
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
