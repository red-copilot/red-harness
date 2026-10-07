from __future__ import annotations

from pathlib import Path

import yaml
from pydantic import BaseModel, Field


class GatewayPolicy(BaseModel):
    allowed_tools: set[str] = Field(default_factory=lambda: {"file.read", "file.write"})
    denied_tools: set[str] = Field(default_factory=set)
    max_tool_output_bytes: int = Field(default=65536, ge=1024, le=1048576)
    max_file_write_bytes: int = Field(default=1048576, ge=1, le=16777216)

    def allows(self, tool: str) -> bool:
        return tool in self.allowed_tools and tool not in self.denied_tools


def load_policy(path: str | Path | None) -> GatewayPolicy:
    if path is None:
        return GatewayPolicy()
    policy_path = Path(path)
    with policy_path.open("r", encoding="utf-8") as handle:
        data = yaml.safe_load(handle)
    if data is None:
        data = {}
    if not isinstance(data, dict):
        raise TypeError(f"{policy_path} must contain a YAML object")
    return GatewayPolicy.model_validate(data)
