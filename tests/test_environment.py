from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from harness.environment import DockerComposeEnvironment, EnvironmentError
from harness.models import EnvironmentSpec
from harness.trace import TraceRecorder


def test_compose_resume_reuses_existing_services_without_rebuilding(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    import harness.environment as environment_module

    monkeypatch.setattr(environment_module.shutil, "which", lambda _name: "/usr/bin/docker")
    manifest = tmp_path / "compose.yaml"
    manifest.write_text("services:\n  app:\n    image: alpine\n", encoding="utf-8")
    env = DockerComposeEnvironment(
        EnvironmentSpec(provider="docker-compose", manifest="compose.yaml"),
        tmp_path,
        "run-resume",
        TraceRecorder(tmp_path / "trace.jsonl", "run-resume", "task"),
        resume_existing=True,
    )
    calls: list[tuple[str, ...]] = []

    def compose(*args: str, **_kwargs):
        calls.append(args)
        output = "app\n" if args[:1] in {("config",), ("ps",)} else ""
        return subprocess.CompletedProcess(args, 0, stdout=output, stderr="")

    monkeypatch.setattr(env, "_compose", compose)

    handle = env.start()

    assert calls == [("config", "--services"), ("ps", "--all", "--services"), ("start",)]
    assert handle.project_name == env.project_name


def test_compose_resume_refuses_missing_services(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    import harness.environment as environment_module

    monkeypatch.setattr(environment_module.shutil, "which", lambda _name: "/usr/bin/docker")
    manifest = tmp_path / "compose.yaml"
    manifest.write_text("services:\n  app:\n    image: alpine\n", encoding="utf-8")
    env = DockerComposeEnvironment(
        EnvironmentSpec(provider="docker-compose", manifest="compose.yaml"),
        tmp_path,
        "run-resume",
        TraceRecorder(tmp_path / "trace.jsonl", "run-resume", "task"),
        resume_existing=True,
    )
    calls: list[tuple[str, ...]] = []

    def compose(*args: str, **_kwargs):
        calls.append(args)
        if args[:1] == ("config",):
            return subprocess.CompletedProcess(args, 0, stdout="app\n", stderr="")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(env, "_compose", compose)

    with pytest.raises(EnvironmentError, match="existing Compose services"):
        env.start()

    assert calls == [("config", "--services"), ("ps", "--all", "--services")]


def test_compose_interrupted_resume_stops_services_without_removing_volumes(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    import harness.environment as environment_module

    monkeypatch.setattr(environment_module.shutil, "which", lambda _name: "/usr/bin/docker")
    manifest = tmp_path / "compose.yaml"
    manifest.write_text("services:\n  app:\n    image: alpine\n", encoding="utf-8")
    env = DockerComposeEnvironment(
        EnvironmentSpec(provider="docker-compose", manifest="compose.yaml"),
        tmp_path,
        "run-resume",
        TraceRecorder(tmp_path / "trace.jsonl", "run-resume", "task"),
        resume_existing=True,
    )
    calls: list[tuple[str, ...]] = []

    def compose(*args: str, **_kwargs):
        calls.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(env, "_compose", compose)

    env.stop(preserve_state=True)

    assert calls == [("stop",)]
