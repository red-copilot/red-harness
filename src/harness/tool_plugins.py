"""Opt-in Gateway tool implementations with explicit process/network boundaries."""

from __future__ import annotations

import asyncio
import ipaddress
import os
import shutil
import signal
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

import httpx

from .tool_adapter import ToolExecutionContext, ToolExecutionResult


def _string(value: Any, name: str, *, max_length: int) -> str:
    if not isinstance(value, str) or not value or len(value) > max_length or "\x00" in value:
        raise ValueError(f"{name} must be a non-empty string of at most {max_length} characters")
    return value


class SandboxedProcessToolAdapter:
    """Run an argv directly in a no-network bubblewrap namespace.

    The tool is intentionally not registered by default. Callers must inject it
    into the Gateway and separately authorize ``process.run`` in GatewayPolicy.
    """

    tool_name = "process.run"

    def __init__(
        self,
        *,
        bubblewrap: str | None = None,
        prlimit: str | None = None,
        timeout_seconds: float = 30.0,
        output_limit_bytes: int = 65536,
        memory_limit_bytes: int = 1024 * 1024 * 1024,
        cpu_limit_seconds: int = 30,
    ) -> None:
        self.bubblewrap = bubblewrap or shutil.which("bwrap")
        self.prlimit = prlimit or shutil.which("prlimit")
        self.timeout_seconds = timeout_seconds
        self.output_limit_bytes = output_limit_bytes
        self.memory_limit_bytes = memory_limit_bytes
        self.cpu_limit_seconds = cpu_limit_seconds
        if timeout_seconds <= 0 or output_limit_bytes < 1:
            raise ValueError("process timeout and output limit must be positive")
        if memory_limit_bytes < 64 * 1024 * 1024 or cpu_limit_seconds < 1:
            raise ValueError("process resource limits are too small")

    async def execute(
        self,
        arguments: Mapping[str, Any],
        context: ToolExecutionContext,
    ) -> ToolExecutionResult:
        if not self.bubblewrap or not self.prlimit:
            raise RuntimeError("bubblewrap and prlimit are required for process.run")
        raw_argv = arguments.get("argv")
        if (
            not isinstance(raw_argv, Sequence)
            or isinstance(raw_argv, (str, bytes))
            or not 1 <= len(raw_argv) <= 128
        ):
            raise ValueError("argv must be an array containing 1 to 128 arguments")
        argv = [_string(item, "argv item", max_length=4096) for item in raw_argv]
        if sum(len(item.encode("utf-8")) for item in argv) > 65536:
            raise ValueError("argv exceeds the 65536-byte limit")

        workspace = self._safe_root(context.workspace, "workspace")
        task_dir = self._safe_root(context.task_dir, "task directory")
        cwd_arg = arguments.get("cwd", ".")
        cwd = _string(cwd_arg, "cwd", max_length=4096)
        relative_cwd = Path(cwd)
        if relative_cwd.is_absolute() or ".." in relative_cwd.parts:
            raise ValueError("cwd must remain within the Agent workspace")
        working_dir = (workspace / relative_cwd).resolve(strict=True)
        if not working_dir.is_dir() or not working_dir.is_relative_to(workspace):
            raise ValueError("cwd must be a directory within the Agent workspace")
        sandbox_cwd = "/workspace" + (
            "/" + working_dir.relative_to(workspace).as_posix()
            if working_dir != workspace
            else ""
        )

        command = self._command(argv, workspace, task_dir, sandbox_cwd)
        process = await asyncio.create_subprocess_exec(
            *command,
            stdin=asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env={"PATH": "/usr/bin:/bin", "HOME": "/tmp", "LANG": "C.UTF-8"},
            start_new_session=True,
        )
        overflow = asyncio.Event()
        assert process.stdout is not None and process.stderr is not None
        stdout_task = asyncio.create_task(self._read_bounded(process.stdout, overflow))
        stderr_task = asyncio.create_task(self._read_bounded(process.stderr, overflow))
        wait_task = asyncio.create_task(process.wait())
        cancel_task = asyncio.create_task(context.cancelled.wait())
        overflow_task = asyncio.create_task(overflow.wait())
        timed_out = False
        cancelled = False
        try:
            done, _ = await asyncio.wait(
                {wait_task, cancel_task, overflow_task},
                timeout=self.timeout_seconds,
                return_when=asyncio.FIRST_COMPLETED,
            )
            cancelled = cancel_task in done and context.cancelled.is_set()
            timed_out = not done
            if cancelled or timed_out or overflow.is_set():
                await self._terminate_group(process)
            await wait_task
            stdout, stdout_truncated = await stdout_task
            stderr, stderr_truncated = await stderr_task
        except asyncio.CancelledError:
            context.cancelled.set()
            await self._terminate_group(process)
            for task in (wait_task, stdout_task, stderr_task):
                if not task.done():
                    task.cancel()
            await asyncio.gather(wait_task, stdout_task, stderr_task, return_exceptions=True)
            raise
        finally:
            cancel_task.cancel()
            overflow_task.cancel()
            await asyncio.gather(cancel_task, overflow_task, return_exceptions=True)

        if cancelled:
            raise asyncio.CancelledError
        return ToolExecutionResult(
            output={
                "exit_code": process.returncode,
                "stdout": stdout.decode("utf-8", errors="replace"),
                "stderr": stderr.decode("utf-8", errors="replace"),
                "timed_out": timed_out,
                "truncated": stdout_truncated or stderr_truncated or overflow.is_set(),
            },
            metadata={"sandbox": "bubblewrap", "network": "none"},
        )

    @staticmethod
    def _safe_root(path: Path, name: str) -> Path:
        if path.is_symlink() or not path.is_dir():
            raise ValueError(f"{name} must be a real directory")
        return path.resolve(strict=True)

    def _command(
        self, argv: list[str], workspace: Path, task_dir: Path, sandbox_cwd: str
    ) -> list[str]:
        assert self.bubblewrap is not None and self.prlimit is not None
        command = [
            self.bubblewrap,
            "--die-with-parent",
            "--unshare-all",
            "--cap-drop",
            "ALL",
        ]
        for path in ("/bin", "/usr", "/lib", "/lib64"):
            source = Path(path)
            if source.exists():
                command.extend(["--dir", path, "--ro-bind", path, path])
        command.extend(
            [
                "--dev",
                "/dev",
                "--proc",
                "/proc",
                "--tmpfs",
                "/tmp",
                "--dir",
                "/workspace",
                "--bind",
                str(workspace),
                "/workspace",
                "--dir",
                "/task",
                "--ro-bind",
                str(task_dir),
                "/task",
                "--chdir",
                sandbox_cwd,
                "--clearenv",
                "--setenv",
                "PATH",
                "/usr/bin:/bin",
                "--setenv",
                "HOME",
                "/tmp",
                "--setenv",
                "LANG",
                "C.UTF-8",
                "--",
                self.prlimit,
                f"--cpu={self.cpu_limit_seconds}",
                f"--as={self.memory_limit_bytes}",
                "--fsize=67108864",
                "--nproc=64",
                "--nofile=128",
                "--",
                *argv,
            ]
        )
        return command

    async def _read_bounded(
        self, stream: asyncio.StreamReader, overflow: asyncio.Event
    ) -> tuple[bytes, bool]:
        output = bytearray()
        truncated = False
        while chunk := await stream.read(65536):
            remaining = self.output_limit_bytes - len(output)
            if remaining > 0:
                output.extend(chunk[:remaining])
            if len(chunk) > max(remaining, 0):
                truncated = True
                overflow.set()
                # Yield so the supervisor can observe overflow and kill the group;
                # a busy producer could otherwise starve the event loop.
                await asyncio.sleep(0)
        return bytes(output), truncated

    @staticmethod
    async def _terminate_group(process: asyncio.subprocess.Process, *, grace: float = 0.5) -> None:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        if process.returncode is None:
            try:
                await asyncio.wait_for(process.wait(), timeout=grace)
            except TimeoutError:
                pass
        try:
            # Bubblewrap's PID namespace may have children after its leader exits.
            # Kill the entire host process group even when the leader is already reaped.
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        if process.returncode is None:
            await process.wait()


class AuthorizedHTTPToolAdapter:
    """Make bounded HTTP requests to literal IPs inside explicit CIDR/port policy."""

    tool_name = "network.request"

    def __init__(
        self,
        *,
        allowed_cidrs: Sequence[str],
        allowed_ports: Sequence[int] = (80, 443),
        methods: Sequence[str] = ("GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"),
        timeout_seconds: float = 15.0,
        max_response_bytes: int = 65536,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        if not allowed_cidrs:
            raise ValueError("network.request requires at least one authorized CIDR")
        self.allowed_networks = tuple(ipaddress.ip_network(item, strict=False) for item in allowed_cidrs)
        self.allowed_ports = frozenset(allowed_ports)
        self.methods = frozenset(method.upper() for method in methods)
        self.timeout_seconds = timeout_seconds
        self.max_response_bytes = max_response_bytes
        self.transport = transport
        if not self.allowed_ports or any(not 1 <= port <= 65535 for port in self.allowed_ports):
            raise ValueError("authorized ports must be between 1 and 65535")
        if timeout_seconds <= 0 or max_response_bytes < 1:
            raise ValueError("network timeout and response limit must be positive")

    async def execute(
        self,
        arguments: Mapping[str, Any],
        context: ToolExecutionContext,
    ) -> ToolExecutionResult:
        method = _string(arguments.get("method", "GET"), "method", max_length=16).upper()
        if method not in self.methods:
            raise ValueError("HTTP method is not enabled for this network adapter")
        raw_url = _string(arguments.get("url"), "url", max_length=4096)
        parsed = urlsplit(raw_url)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise ValueError("network URL must use HTTP(S) and a literal IP address")
        if parsed.username or parsed.password or parsed.fragment:
            raise ValueError("network URL cannot include user info or a fragment")
        try:
            destination = ipaddress.ip_address(parsed.hostname)
            port = parsed.port or (443 if parsed.scheme == "https" else 80)
        except ValueError as exc:
            raise ValueError("network URL must use a literal IP address and valid port") from exc
        if port not in self.allowed_ports or not any(
            destination.version == network.version and destination in network
            for network in self.allowed_networks
        ):
            raise PermissionError("network destination is outside the authorized CIDR/port set")

        headers_arg = arguments.get("headers", {})
        if not isinstance(headers_arg, Mapping) or len(headers_arg) > 64:
            raise ValueError("headers must be an object with at most 64 entries")
        headers: dict[str, str] = {}
        for key, value in headers_arg.items():
            name = _string(key, "header name", max_length=256)
            content = _string(value, "header value", max_length=8192)
            if "\r" in name or "\n" in name or "\r" in content or "\n" in content:
                raise ValueError("header values cannot contain line breaks")
            headers[name] = content
        body = arguments.get("body")
        if body is not None:
            body = _string(body, "body", max_length=1024 * 1024)

        async def send() -> ToolExecutionResult:
            async with httpx.AsyncClient(
                timeout=self.timeout_seconds,
                follow_redirects=False,
                trust_env=False,
                transport=self.transport,
            ) as client, client.stream(
                method, raw_url, headers=headers, content=body
            ) as response:
                output = bytearray()
                truncated = False
                async for chunk in response.aiter_bytes():
                    room = self.max_response_bytes - len(output)
                    if room > 0:
                        output.extend(chunk[:room])
                    if len(chunk) > max(room, 0):
                        truncated = True
                        break
                return ToolExecutionResult(
                    output={
                        "status_code": response.status_code,
                        "content_type": response.headers.get("content-type"),
                        "body": output.decode("utf-8", errors="replace"),
                        "truncated": truncated,
                        "redirected": 300 <= response.status_code < 400,
                    },
                    metadata={
                        "destination": str(destination),
                        "port": port,
                        "method": method,
                    },
                )

        request_task = asyncio.create_task(send())
        cancel_task = asyncio.create_task(context.cancelled.wait())
        try:
            done, _ = await asyncio.wait(
                {request_task, cancel_task}, return_when=asyncio.FIRST_COMPLETED
            )
            if cancel_task in done and context.cancelled.is_set():
                request_task.cancel()
                await asyncio.gather(request_task, return_exceptions=True)
                raise asyncio.CancelledError
            return await request_task
        except asyncio.CancelledError:
            request_task.cancel()
            await asyncio.gather(request_task, return_exceptions=True)
            raise
        finally:
            cancel_task.cancel()
            await asyncio.gather(cancel_task, return_exceptions=True)
