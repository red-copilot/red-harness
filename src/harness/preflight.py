"""Local, offline readiness checks for task and Agent configurations."""

from __future__ import annotations

import os
import shutil
import subprocess
from importlib.util import find_spec
from pathlib import Path

from pydantic import BaseModel

from .agent_workspace import validate_agent_task_inputs
from .models import AgentSpec, TaskSpec


class PreflightCheck(BaseModel):
    name: str
    ok: bool
    detail: str


class PreflightReport(BaseModel):
    ready: bool
    checks: list[PreflightCheck]

    def by_name(self, name: str) -> PreflightCheck:
        return next(check for check in self.checks if check.name == name)


def _local_image_available(image: str) -> bool:
    docker = shutil.which("docker")
    if docker is None:
        return False
    try:
        result = subprocess.run(
            [docker, "image", "inspect", image],
            check=False,
            capture_output=True,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    return result.returncode == 0


def run_preflight(
    task: TaskSpec,
    agent: AgentSpec,
    *,
    task_path: str | Path,
    require_tsec: bool = False,
    gateway_enabled: bool = False,
    gateway_mode: str = "host",
    gateway_image: str | None = None,
    model_upstream: str | None = None,
) -> PreflightReport:
    """Check only local prerequisites; this function never contacts a registry."""
    source = Path(task_path).resolve()
    task_root = source.parent
    checks: list[PreflightCheck] = [
        PreflightCheck(name="task_input", ok=source.is_file(), detail=str(source))
    ]

    if task.environment.provider == "docker-compose":
        manifest = task.environment.manifest
        manifest_path = (task_root / manifest).resolve() if manifest else task_root
        contained = manifest_path == task_root or task_root in manifest_path.parents
        ok = bool(manifest and contained and manifest_path.is_file())
        checks.append(
            PreflightCheck(
                name="environment_manifest",
                ok=ok,
                detail=str(manifest_path)
                if ok
                else "environment manifest is missing or outside task directory",
            )
        )

    if task.verification.type == "python":
        verifier = (task_root / task.verification.entrypoint).resolve()
        contained = verifier == task_root or task_root in verifier.parents
        ok = contained and verifier.is_file()
        checks.append(
            PreflightCheck(
                name="verifier",
                ok=ok,
                detail=str(verifier)
                if ok
                else "Python verifier is missing or outside task directory",
            )
        )
    else:
        image = task.verification.image or ""
        ok = _local_image_available(image)
        checks.append(
            PreflightCheck(
                name="verifier_image",
                ok=ok,
                detail=(
                    f"local image available: {image}"
                    if ok
                    else f"local image unavailable: {image}; build or load it before running"
                ),
            )
        )

    if require_tsec:
        sdk_available = find_spec("tsec_benchmark") is not None
        checks.append(
            PreflightCheck(
                name="tsec_sdk",
                ok=sdk_available,
                detail=(
                    "tsec-benchmark SDK is installed"
                    if sdk_available
                    else 'tsec-benchmark SDK is unavailable; install red-harness with ".[tsec]"'
                ),
            )
        )
        base_url_present = bool(os.environ.get("BENCHMARK_BASE_URL"))
        token_present = bool(os.environ.get("BENCHMARK_TOKEN"))
        checks.extend(
            (
                PreflightCheck(
                    name="tsec_base_url",
                    ok=base_url_present,
                    detail=(
                        "BENCHMARK_BASE_URL is configured"
                        if base_url_present
                        else "BENCHMARK_BASE_URL is missing"
                    ),
                ),
                PreflightCheck(
                    name="tsec_credential",
                    ok=token_present,
                    detail=(
                        "BENCHMARK_TOKEN is configured (value is not displayed)"
                        if token_present
                        else "BENCHMARK_TOKEN is missing (value is not displayed)"
                    ),
                ),
            )
        )

    if agent.type == "cli":
        executable = agent.command[0] if agent.command else ""
        available = bool(executable and shutil.which(executable))
        checks.append(
            PreflightCheck(
                name="agent_command",
                ok=available,
                detail=(
                    f"executable available: {executable}"
                    if available
                    else f"executable unavailable: {executable or '(empty command)'}"
                ),
            )
        )
    else:
        image = agent.image or ""
        available = _local_image_available(image)
        checks.append(
            PreflightCheck(
                name="agent_image",
                ok=available,
                detail=(
                    f"local image available: {image}"
                    if available
                    else f"local image unavailable: {image}; build or load it before running"
                ),
            )
        )

    if agent.type in {"docker", "pi"}:
        try:
            validate_agent_task_inputs(
                task_dir=task_root,
                verifier_entrypoint=task.verification.entrypoint,
            )
        except (OSError, ValueError) as exc:
            checks.append(
                PreflightCheck(
                    name="agent_task_inputs",
                    ok=False,
                    detail=f"Agent task input snapshot is unsafe: {exc}",
                )
            )
        else:
            checks.append(
                PreflightCheck(
                    name="agent_task_inputs",
                    ok=True,
                    detail="Agent task input tree is readable and within snapshot limits",
                )
            )

    if agent.type == "pi" and not gateway_enabled and agent.pi is not None and agent.pi.provider:
        key_names = {
            "openai": "OPENAI_API_KEY",
            "anthropic": "ANTHROPIC_API_KEY",
            "google": "GEMINI_API_KEY",
        }
        key_name = key_names.get(agent.pi.provider.lower())
        if key_name:
            present = bool(agent.env.get(key_name, "").strip()) or (
                key_name in agent.pi.env_passthrough
                and bool(os.environ.get(key_name, "").strip())
            )
            checks.append(
                PreflightCheck(
                    name="model_credential",
                    ok=present,
                    detail=(
                        f"{key_name} is available to the Pi container"
                        if present
                        else f"configure {key_name} in pi.env_passthrough or inject a Gateway"
                    ),
                )
            )

    profile = agent.network_profile
    strict_profiles = {"offline", "target-only", "model-allowed", "fully-offline"}
    host_agent = agent.type == "cli" and profile in strict_profiles
    legacy_advisory = profile == "benchmark-only"
    network_ready = not host_agent
    topology_errors: list[str] = []
    if gateway_enabled and gateway_mode not in {"host", "sidecar"}:
        network_ready = False
        topology_errors.append("gateway mode must be host or sidecar")
    if gateway_enabled and gateway_mode == "sidecar":
        if not gateway_image:
            network_ready = False
            topology_errors.append("sidecar Gateway mode requires a local Gateway image")
        else:
            sidecar_available = _local_image_available(gateway_image)
            checks.append(
                PreflightCheck(
                    name="gateway_image",
                    ok=sidecar_available,
                    detail=(
                        f"local image available: {gateway_image}"
                        if sidecar_available
                        else f"local image unavailable: {gateway_image}; build or load it before running"
                    ),
                )
            )
    if profile in {"offline", "fully-offline"} and gateway_enabled:
        network_ready = False
        topology_errors.append("fully-offline networking cannot use a Gateway")
    if profile == "target-only":
        if task.environment.provider != "docker-compose":
            network_ready = False
            topology_errors.append("target-only requires a Docker Compose environment network")
        if gateway_enabled:
            network_ready = False
            topology_errors.append("target-only networking cannot use a Gateway")
    if profile == "model-allowed":
        if agent.type != "docker":
            network_ready = False
            topology_errors.append("model-allowed currently requires a Docker Agent")
        if not gateway_enabled or gateway_mode != "sidecar" or not gateway_image:
            network_ready = False
            topology_errors.append(
                "model-allowed requires --gateway --gateway-mode sidecar --gateway-image"
            )
        elif not model_upstream:
            network_ready = False
            topology_errors.append("model-allowed requires --model-upstream")
    if agent.type == "pi" and gateway_enabled and not model_upstream:
        network_ready = False
        topology_errors.append("Pi Gateway mode requires --model-upstream for model access")
    effective_network = (
        "none"
        if profile in {"offline", "fully-offline"}
        else "environment (must be Docker-internal)"
        if profile == "target-only"
        else "internal Gateway + internal environment (sidecar required)"
        if profile == "model-allowed"
        else agent.network
    )
    network_detail = (
        f"network_profile={profile}; configured_network={agent.network}; "
        f"effective_agent_network={effective_network}; "
        + (
            "strict profiles require a Docker Agent"
            if host_agent
            else "legacy profile is advisory and does not isolate public egress"
            if legacy_advisory
            else "strict network topology is checked at runtime; no connectivity probe performed"
        )
        + (f"; preflight errors: {'; '.join(topology_errors)}" if topology_errors else "")
    )
    checks.append(
        PreflightCheck(
            name="network_policy",
            ok=network_ready,
            detail=network_detail,
        )
    )
    return PreflightReport(ready=all(check.ok for check in checks), checks=checks)
