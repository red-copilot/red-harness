from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from experiments.pi_ablation.native_pi import command_for_pi, native_prompt


def spec(*, mode="json", skills=False):
    pi = SimpleNamespace(mode=mode, extensions=False, context_files=False,
                         skills=skills, mcp=False, prompt_templates=False,
                         cap_add=["NET_RAW"], env_passthrough=["OPENAI_API_KEY"],
                         binary="pi", launcher_args=[], tools=["bash"],
                         provider="openai", model="test-model", thinking="medium")
    return SimpleNamespace(type="pi", network="host", network_profile="benchmark-only",
                           pi=pi, image="harness/pi-kali:local", env={})


def test_native_prompt_omits_world_protocol():
    session = SimpleNamespace(
        objective=SimpleNamespace(description="Solve authorized task"),
        targets=[SimpleNamespace(address="127.0.0.1:8080")],
    )
    prompt = native_prompt(session)
    assert "127.0.0.1:8080" in prompt
    assert "HARNESS_WORLD_CONTEXT" not in prompt
    assert "HARNESS_SUBMISSION_INBOX" not in prompt
    assert "solver.replan" not in prompt


def test_native_command_has_no_harness_control_paths(tmp_path: Path):
    with patch.dict("os.environ", {"OPENAI_API_KEY": "dummy"}):
        cmd = command_for_pi(spec(), "Solve authorized task", tmp_path, "test_container")
    joined = " ".join(cmd)
    assert "--mode json" in joined
    for forbidden in ("WORLD_CONTEXT", "PROGRESS_FILE", "SUBMISSION_INBOX",
                      "world.db", "solver", "BENCHMARK_TOKEN", "dummy"):
        assert forbidden not in joined
    assert cmd[-1] == "Solve authorized task"


@pytest.mark.parametrize("mode,skills", [("rpc", False), ("json", True)])
def test_rejects_nonbaseline_features(tmp_path: Path, mode: str, skills: bool):
    with pytest.raises(ValueError):
        command_for_pi(spec(mode=mode, skills=skills), "task", tmp_path, "test_container")
