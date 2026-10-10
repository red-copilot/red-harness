"""Regression tests for repeated Agent workspace synchronization."""

import os

from harness.agent_workspace import sync_agent_workspace


def test_repeated_sync_does_not_replace_unchanged_views(tmp_path):
    run_dir = tmp_path / "run"
    workspace = tmp_path / "workspace"
    run_dir.mkdir()
    workspace.mkdir()
    (run_dir / "progress.json").write_text('{"step":1}', encoding="utf-8")
    (run_dir / "world.context.txt").write_text("known fact", encoding="utf-8")

    sync_agent_workspace(run_dir=run_dir, workspace=workspace)
    first = {
        name: (workspace / name).stat().st_ino
        for name in ("progress.json", "world.context.txt")
    }
    sync_agent_workspace(run_dir=run_dir, workspace=workspace)
    assert first == {
        name: (workspace / name).stat().st_ino
        for name in first
    }
    (run_dir / "progress.json").write_text('{"step":2}', encoding="utf-8")
    sync_agent_workspace(run_dir=run_dir, workspace=workspace)
    assert (workspace / "progress.json").read_text() == '{"step":2}'
    assert (workspace / "world.context.txt").stat().st_ino == first["world.context.txt"]


def test_sync_does_not_follow_agent_controlled_symlink(tmp_path):
    run_dir = tmp_path / "run"
    workspace = tmp_path / "workspace"
    run_dir.mkdir()
    workspace.mkdir()
    external = tmp_path / "external"
    external.write_text("untrusted", encoding="utf-8")
    (run_dir / "progress.json").write_text("trusted", encoding="utf-8")
    os.symlink(external, workspace / "progress.json")

    sync_agent_workspace(run_dir=run_dir, workspace=workspace)
    assert external.read_text(encoding="utf-8") == "untrusted"
    assert not (workspace / "progress.json").is_symlink()
    assert (workspace / "progress.json").read_text(encoding="utf-8") == "trusted"
