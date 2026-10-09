"""Enforced container network profile requirements."""
from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass

from .agent import AgentError
from .models import AgentSpec

STRICT_PROFILES = {"offline", "target-only", "model-allowed", "fully-offline"}
PROXY_ENV_NAMES = (
    "HTTP_PROXY",
    "HTTPS_PROXY",
    "NO_PROXY",
    "FTP_PROXY",
    "ALL_PROXY",
    "http_proxy",
    "https_proxy",
    "no_proxy",
    "ftp_proxy",
    "all_proxy",
)


def reject_agent_proxy_env(profile: str, names: set[str]) -> None:
    proxy_names = {name.lower() for name in PROXY_ENV_NAMES}
    if profile in STRICT_PROFILES and any(name.lower() in proxy_names for name in names):
        raise AgentError("strict network profiles cannot pass Agent-configured proxy variables")


def proxy_environment_overrides(profile: str) -> list[str]:
    """Clear Docker CLI proxy injection when a strict network profile is active."""
    if profile not in STRICT_PROFILES:
        return []
    return [argument for name in PROXY_ENV_NAMES for argument in ("-e", f"{name}=")]


@dataclass(frozen=True)
class EnforcedNetworks:
    """Network IDs validated for a strict profile, ready for Docker commands."""

    primary: str
    environment: str | None = None
    gateway: str | None = None


def _require_internal_network(name: str, *, purpose: str) -> str:
    """Validate a network and return its immutable Docker ID, not its mutable name."""
    try:
        inspected = subprocess.run(
            ["docker", "network", "inspect", "--format", "{{.Id}} {{.Internal}}", name],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise AgentError(f"cannot verify internal Docker network for {purpose}") from exc
    fields = inspected.stdout.split()
    if (
        inspected.returncode != 0
        or len(fields) != 2
        or re.fullmatch(r"[0-9a-fA-F]{64}", fields[0]) is None
        or fields[1].lower() != "true"
    ):
        raise AgentError(
            f"{purpose} requires Docker network {name!r} to have Internal=true"
        )
    return fields[0]


def enforced_network(
    spec: AgentSpec,
    *,
    environment_network: str | None,
    gateway_network: str | None,
    gateway_enabled: bool,
) -> EnforcedNetworks | None:
    """Return the enforced primary network, or None for legacy profiles.

    Strict profiles fail closed on host CLI Agents, unverified target networks,
    or missing model Gateway routing. ``benchmark-only`` remains a legacy,
    advisory alias for compatibility and must not be treated as isolation.
    """
    profile = spec.network_profile
    if profile not in STRICT_PROFILES:
        return None
    if spec.type not in {"docker", "pi"}:
        raise AgentError(f"network profile {profile!r} requires a Docker Agent")

    if profile in {"offline", "fully-offline"}:
        if gateway_enabled:
            raise AgentError("fully-offline networking is incompatible with Gateway access")
        return EnforcedNetworks(primary="none")

    if profile == "target-only":
        if not environment_network:
            raise AgentError("network profile 'target-only' requires an environment network")
        environment_id = _require_internal_network(environment_network, purpose=profile)
        if gateway_enabled:
            raise AgentError("target-only networking is incompatible with Gateway access")
        return EnforcedNetworks(primary=environment_id, environment=environment_id)

    if not gateway_enabled or not gateway_network:
        raise AgentError("model-allowed networking requires a sidecar model Gateway")
    if spec.type != "docker":
        raise AgentError("model-allowed networking currently requires a Docker Agent")
    gateway_id = _require_internal_network(gateway_network, purpose=profile)
    environment_id = (
        _require_internal_network(environment_network, purpose=profile)
        if environment_network
        else None
    )
    return EnforcedNetworks(
        primary=gateway_id,
        environment=environment_id,
        gateway=gateway_id,
    )
