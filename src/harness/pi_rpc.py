"""Interactive Pi RPC in Kali Docker, with trusted protocol telemetry.

Only records received on Pi's stdout are treated as tool/model telemetry.
Agent-written JSONL is an untrusted semantic-claims channel.
"""
from __future__ import annotations

import asyncio
import json
import os
import subprocess
import time
import uuid
from pathlib import Path
from typing import TYPE_CHECKING

from .agent import AgentError, AgentResult
from .budget import BudgetMonitor
from .models import TaskSpec
from .pi_adapter import PiAdapter
from .session import AgentCheckpoint, AgentEvent, AgentObservation

if TYPE_CHECKING:
    from .pi_container import ContainerPiAdapter


AGENT_CLAIMS = frozenset({
    "action.intent", "world.observe", "world.hypothesis",
    "world.capability", "world.artifact", "world.failure",
})
STEER_FEEDBACK = frozenset({"benchmark.feedback", "solver.verification", "solver.plan.updated"})


class _PiStreamNormalizer:
    """Reuse the JSON Pi normalizer without reading Agent-writable event files."""

    def __init__(self, adapter: ContainerPiAdapter):
        self.pi = adapter.pi
        self._tool_started: dict[str, float] = {}
        self._events: list[AgentEvent] = []

    def _append_event(self, _path: Path, event_type: str, data: dict) -> None:
        self._events.append(AgentEvent(type=event_type, data=data))

    def translate(self, raw: str, *, account_model: bool) -> list[AgentEvent]:
        PiAdapter._translate_line(
            self, raw, event_file=Path("/dev/null"), account_model=account_model,
        )
        events, self._events = self._events, []
        return events


class ContainerPiRpcSession:
    """Duplex JSONL session; stdin/stdout belong to Docker-attached Pi RPC."""

    def __init__(
        self, adapter: ContainerPiAdapter, *, task: TaskSpec, run_dir: Path,
        container_name: str, process: asyncio.subprocess.Process,
        gateway_enabled: bool, stdout_handle, stderr_handle,
    ) -> None:
        self.adapter = adapter
        self.task = task
        self.run_dir = run_dir
        self.container_name = container_name
        self.process = process
        self.stdout_handle = stdout_handle
        self.stderr_handle = stderr_handle
        self.event_path = run_dir / "events.jsonl"
        self.feedback_path = run_dir / "agent.feedback.jsonl"
        self._event_offset = 0
        self._feedback_offset = 0
        self._events: asyncio.Queue[AgentEvent | None] = asyncio.Queue()
        self._responses: dict[str, asyncio.Future[dict]] = {}
        self._write_lock = asyncio.Lock()
        self._normalizer = _PiStreamNormalizer(adapter)
        self._budget = BudgetMonitor(run_dir / "unused.rpc.telemetry", task.budgets, adapter.trace)
        self._account_model = not gateway_enabled
        self._started = time.monotonic()
        self._running = True
        self._pending_turns = 1
        self._closed = False
        self._timed_out = False
        self._error: Exception | None = None
        self._result: AgentResult | None = None
        self._reader_task = asyncio.create_task(self._read_stdout())

    @classmethod
    async def start(
        cls, adapter: ContainerPiAdapter, *, task: TaskSpec, task_dir: Path,
        run_dir: Path, environment_project: str | None,
        environment_network: str | None, seed: int,
        gateway_url: str | None = None, gateway_token: str | None = None,
        gateway_network: str | None = None,
    ) -> ContainerPiRpcSession:
        gateway_enabled = bool(gateway_url and gateway_token)
        if gateway_enabled and (
            adapter.spec.network == "none" or adapter.spec.network_profile == "offline"
        ):
            raise AgentError("Pi offline networking is incompatible with per-run Gateway")
        if adapter.spec.network == "host" and gateway_url and gateway_network is None:
            gateway_url = gateway_url.replace("host.docker.internal", "127.0.0.1")
        sidecar_gateway = gateway_enabled and gateway_network is not None
        if sidecar_gateway and adapter.spec.network == "host":
            raise AgentError("Pi network:host is incompatible with sidecar Gateway")
        if adapter.spec.network_profile == "offline":
            network = "none"
        elif sidecar_gateway:
            network = str(gateway_network)
        elif adapter.spec.network == "host":
            network = "host"
        elif adapter.spec.network == "environment":
            network = environment_network or "bridge"
        else:
            network = "none"

        # Protect canonical DB, progress, trace and feedback with a read-only root
        # mount. Agent-generated JSONL is confined to the writable input mount.
        inbox = run_dir / "agent-input"
        inbox.mkdir(mode=0o700, exist_ok=True)
        for filename in ("events.jsonl", "world.inbox.jsonl", "submission.inbox.jsonl"):
            destination = run_dir / filename
            target = inbox / filename
            if destination.exists() or destination.is_symlink():
                raise AgentError(f"RPC mailbox already exists: {destination}")
            target.touch(mode=0o600)
            destination.symlink_to(target.relative_to(run_dir))
        adapter._prepare_agent_dir(
            run_dir=run_dir, gateway_url=gateway_url, gateway_token=gateway_token,
        )
        (run_dir / "home").mkdir(exist_ok=True)
        (run_dir / "pi-sessions").mkdir(exist_ok=True)
        (run_dir / "workspace").mkdir(exist_ok=True)
        name = ("harness_pi_" + run_dir.name.lower()).replace("-", "_")[:63]
        command = adapter._container_command(
            task, task_dir=task_dir, run_dir=run_dir,
            container_name=name, network=network,
            environment_project=environment_project, seed=seed,
            gateway_url=gateway_url, gateway_token=gateway_token,
            host_gateway=gateway_enabled and not sidecar_gateway and network != "host",
        )
        # The Docker root mount is read-only; only explicit Agent-owned paths
        # are writable. No Docker socket or Harness state DB is mounted rw.
        root_mount = f"{run_dir.resolve()}:/run/harness:rw"
        assert root_mount in command
        command[command.index(root_mount)] = f"{run_dir.resolve()}:/run/harness:ro"
        mounts = [
            ("agent-input", "input"),
            ("pi-agent", "pi-agent"),
            ("pi-sessions", "pi-sessions"),
            ("home", "home"),
            ("workspace", "workspace"),
        ]
        for source, destination in mounts:
            command.extend(["-v", f"{(run_dir / source).resolve()}:/run/harness/{destination}:rw"])
        command[command.index("-w") + 1] = "/run/harness/workspace"
        for old, new in (
            ("HARNESS_EVENT_FILE=/run/harness/events.jsonl",
             "HARNESS_EVENT_FILE=/run/harness/input/events.jsonl"),
            ("HARNESS_WORLD_INBOX=/run/harness/world.inbox.jsonl",
             "HARNESS_WORLD_INBOX=/run/harness/input/world.inbox.jsonl"),
            ("HARNESS_SUBMISSION_INBOX=/run/harness/submission.inbox.jsonl",
             "HARNESS_SUBMISSION_INBOX=/run/harness/input/submission.inbox.jsonl"),
        ):
            assert old in command
            command[command.index(old)] = new
        prompt = command.pop()  # CLI text is not a valid RPC prompt argument.
        command[command.index("--mode") + 1] = "rpc"
        command.insert(2, "-i")  # Keep container stdin open for RPC commands.
        adapter.trace.emit("agent.started", data={
            "agent_id": adapter.spec.id, "type": "pi", "execution": "docker",
            "pi_protocol": "rpc", "network": network,
            "network_profile": adapter.spec.network_profile,
        })
        await asyncio.to_thread(adapter._docker, command, cwd=task_dir)
        stdout_handle = (run_dir / "agent.stdout.log").open("wb")
        stderr_handle = (run_dir / "agent.stderr.log").open("wb")
        try:
            if sidecar_gateway and environment_network and environment_network != gateway_network:
                await asyncio.to_thread(
                    adapter._docker,
                    ["docker", "network", "connect", environment_network, name],
                    cwd=task_dir,
                )
            process = await asyncio.create_subprocess_exec(
                "docker", "start", "-a", "-i", name,
                stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
                stderr=stderr_handle, cwd=str(task_dir), env=os.environ.copy(),
            )
        except BaseException:
            stdout_handle.close()
            stderr_handle.close()
            await asyncio.to_thread(
                subprocess.run, ["docker", "rm", "-f", name],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
            )
            raise
        session = cls(
            adapter, task=task, run_dir=run_dir, container_name=name,
            process=process, gateway_enabled=gateway_enabled,
            stdout_handle=stdout_handle, stderr_handle=stderr_handle,
        )
        try:
            await session._command("prompt", message=prompt)
        except BaseException:
            await session.close("startup-error")
            raise
        return session

    async def _command(self, command: str, **data) -> dict:
        if self._closed or self.process.stdin is None:
            raise AgentError("Pi RPC stdin is closed")
        req_id = uuid.uuid4().hex
        future = asyncio.get_running_loop().create_future()
        self._responses[req_id] = future
        payload = json.dumps(
            {"id": req_id, "type": command, **data}, ensure_ascii=False,
        ).encode("utf-8") + b"\n"
        try:
            async with self._write_lock:
                self.process.stdin.write(payload)
                await self.process.stdin.drain()
            response = await asyncio.wait_for(future, timeout=15)
            if not response.get("success"):
                raise AgentError(f"Pi RPC {command} failed: {response.get('error')}")
            return response
        finally:
            self._responses.pop(req_id, None)

    async def _read_stdout(self) -> None:
        try:
            assert self.process.stdout is not None
            while raw := await self.process.stdout.readline():
                self.stdout_handle.write(raw)
                self.stdout_handle.flush()
                try:
                    record = json.loads(raw)
                    if not isinstance(record, dict):
                        continue
                except (ValueError, UnicodeDecodeError):
                    self._events.put_nowait(AgentEvent(
                        type="agent.telemetry_error",
                        data={"message": "invalid Pi RPC JSON record"},
                    ))
                    continue
                if record.get("type") == "response":
                    future = self._responses.get(record.get("id"))
                    if future is not None and not future.done():
                        future.set_result(record)
                    continue
                if record.get("type") == "agent_settled":
                    self._running = False
                for event in self._normalizer.translate(
                    raw.decode("utf-8"), account_model=self._account_model,
                ):
                    self.adapter.trace.emit(event.type, actor="agent", data=event.data)
                    self._budget._account(event.type, event.data)
                    self._budget._check()
                    self._events.put_nowait(event)
        except Exception as exc:  # noqa: BLE001 - transport boundary captures protocol failure.
            self._error = exc
        finally:
            for future in self._responses.values():
                if not future.done():
                    future.set_exception(AgentError("Pi RPC process exited"))
            self._events.put_nowait(None)

    def _agent_claims(self) -> list[AgentEvent]:
        if not self.event_path.exists():
            return []
        events = []
        with self.event_path.open("rb") as stream:
            stream.seek(self._event_offset)
            while True:
                line = stream.readline()
                if not line or not line.endswith(b"\n"):
                    break
                self._event_offset = stream.tell()
                if len(line) > 65536:
                    continue
                try:
                    entry = json.loads(line)
                    if not isinstance(entry, dict) or entry.get("type") not in AGENT_CLAIMS:
                        continue
                    payload = entry.get("data")
                    if not isinstance(payload, dict):
                        continue
                    events.append(AgentEvent(
                        type=entry["type"], data=payload, event_id=entry.get("event_id"),
                    ))
                except (ValueError, UnicodeDecodeError):
                    continue
        return events

    async def events(self):
        try:
            while True:
                for claim in self._agent_claims():
                    yield claim
                if self._budget.exceeded:
                    await self.close("budget-exceeded")
                    break
                remaining = self.task.budgets.wall_time - (time.monotonic() - self._started)
                if remaining <= 0:
                    self._timed_out = True
                    self.adapter.trace.emit("budget.exceeded", data={"budget": "wall_time"})
                    await self.close("timeout")
                    break
                try:
                    event = await asyncio.wait_for(
                        self._events.get(), timeout=min(0.1, remaining),
                    )
                except TimeoutError:
                    continue
                if event is None:
                    break
                yield event
                if event.type == "pi.agent_settled":
                    self._pending_turns -= 1
                    if self._pending_turns <= 0:
                        for claim in self._agent_claims():
                            yield claim
                        break
        finally:
            await self.close("session-ended")

    async def observe(self, observation: AgentObservation) -> None:
        record = {
            "id": "feedback_" + uuid.uuid4().hex,
            "type": observation.type, "data": observation.data,
        }
        with self.feedback_path.open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(record, ensure_ascii=False) + "\n")
            stream.flush()
            self._feedback_offset = stream.tell()
        if observation.type not in STEER_FEEDBACK or self._closed:
            return
        if observation.type == "solver.verification" and (
            observation.data.get("status") != "contradicted"
        ):
            return
        if observation.type == "solver.plan.updated" and not (
            observation.data.get("replan_required")
        ):
            return
        message = (
            "Trusted Harness feedback (" + observation.type + "): "
            + json.dumps(observation.data, ensure_ascii=False)
            + "\nReview $HARNESS_WORLD_CONTEXT and $HARNESS_FEEDBACK_FILE. "
              "Reconsider the current hypothesis and continue only within the task scope."
        )
        if self._running:
            await self._command("steer", message=message)
        else:
            self._pending_turns += 1
            self._running = True
            await self._command("prompt", message=message)

    async def checkpoint(self) -> AgentCheckpoint:
        return AgentCheckpoint(
            id="checkpoint_" + uuid.uuid4().hex,
            event_offset=self._event_offset,
            feedback_offset=self._feedback_offset,
        )

    async def close(self, reason: str) -> None:
        if self._closed:
            return
        self._closed = True
        self.adapter.trace.emit("session.close_requested", data={"reason": reason})
        if self.process.stdin is not None and not self.process.stdin.is_closing():
            self.process.stdin.close()
        try:
            await asyncio.wait_for(self.process.wait(), timeout=3)
        except TimeoutError:
            self.process.kill()
            await self.process.wait()
        finally:
            await asyncio.to_thread(
                subprocess.run, ["docker", "rm", "-f", self.container_name],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
            )
            await self._reader_task
            self.stdout_handle.close()
            self.stderr_handle.close()

    async def result(self) -> AgentResult:
        if self._result is not None:
            return self._result
        if not self._closed:
            await self.close("result")
        if self._error is not None:
            raise AgentError(f"Pi RPC stream failed: {self._error}")
        self._result = AgentResult(
            returncode=self.process.returncode or 0,
            timed_out=self._timed_out,
            budget_exceeded=self._budget.exceeded,
            stdout=(self.run_dir / "agent.stdout.log").read_text(
                encoding="utf-8", errors="replace",
            ),
            stderr=(self.run_dir / "agent.stderr.log").read_text(
                encoding="utf-8", errors="replace",
            ),
            metrics=self._budget.metrics,
        )
        self.adapter.trace.emit("agent.finished", data={
            "returncode": self._result.returncode,
            "timed_out": self._result.timed_out,
            "budget_exceeded": self._result.budget_exceeded,
            "metrics": self._result.metrics.as_dict(), "pi_protocol": "rpc",
        })
        return self._result
