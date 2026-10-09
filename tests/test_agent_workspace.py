from pathlib import Path

import pytest

from harness.agent_workspace import validate_agent_task_inputs
from harness.runtime.agent_workspace import (
    create_agent_workspace,
    prepare_agent_task_view,
    prepare_agent_workspace_for_container,
    sync_agent_workspace,
)


def test_agent_workspace_is_separate_and_only_receives_harness_views(tmp_path: Path) -> None:
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    (run_dir / "result.json").write_text('{"success":true}', encoding="utf-8")
    (run_dir / "world.db").write_bytes(b"authoritative")
    (run_dir / "world.context.txt").write_text("context", encoding="utf-8")
    (run_dir / "progress.json").write_text('{"objective_completed":false}', encoding="utf-8")

    workspace = create_agent_workspace(run_dir)
    sync_agent_workspace(run_dir=run_dir, workspace=workspace)

    assert workspace.is_dir()
    assert (workspace / "world.context.txt").read_text(encoding="utf-8") == "context"
    assert (workspace / "progress.json").read_text(
        encoding="utf-8"
    ) == '{"objective_completed":false}'
    assert not (workspace / "result.json").exists()
    assert not (workspace / "world.db").exists()


def test_agent_workspace_refresh_replaces_symlink_without_following_it(tmp_path: Path) -> None:
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    (run_dir / "progress.json").write_text('{"objective_completed":false}', encoding="utf-8")
    workspace = create_agent_workspace(run_dir)
    outside = tmp_path / "outside.json"
    outside.write_text("protected", encoding="utf-8")
    (workspace / "progress.json").symlink_to(outside)

    sync_agent_workspace(run_dir=run_dir, workspace=workspace)

    assert outside.read_text(encoding="utf-8") == "protected"
    assert not (workspace / "progress.json").is_symlink()
    assert (workspace / "progress.json").read_text(
        encoding="utf-8"
    ) == '{"objective_completed":false}'


def test_container_workspace_gets_dedicated_writable_nonroot_owner(tmp_path: Path) -> None:
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    workspace = create_agent_workspace(run_dir)
    (workspace / "events.jsonl").write_text("", encoding="utf-8")

    uid, gid = prepare_agent_workspace_for_container(workspace)

    assert uid != 0
    assert (workspace.stat().st_uid, workspace.stat().st_gid) == (uid, gid)
    event_file = workspace / "events.jsonl"
    assert (event_file.stat().st_uid, event_file.stat().st_gid) == (uid, gid)
    assert event_file.stat().st_mode & 0o200


def test_agent_task_view_hides_verifier_and_never_follows_task_symlinks(tmp_path: Path) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    (task_dir / "task.yaml").write_text("objective: solve", encoding="utf-8")
    (task_dir / "input.txt").write_text("challenge input", encoding="utf-8")
    (task_dir / "verifier.py").write_text("private grader", encoding="utf-8")
    outside = tmp_path / "outside-secret.txt"
    outside.write_text("host secret", encoding="utf-8")
    (task_dir / "secret.txt").symlink_to(outside)

    view = prepare_agent_task_view(
        task_dir=task_dir,
        run_root=tmp_path / "run",
        verifier_entrypoint="verifier.py",
    )

    assert (view / "task.yaml").read_text(encoding="utf-8") == "objective: solve"
    assert (view / "input.txt").read_text(encoding="utf-8") == "challenge input"
    assert not (view / "verifier.py").exists()
    assert not (view / "secret.txt").exists()
    assert outside.read_text(encoding="utf-8") == "host secret"


def test_agent_task_view_refuses_changed_inputs_on_resume(tmp_path: Path) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    (task_dir / "input.txt").write_text("original", encoding="utf-8")
    run_root = tmp_path / "run"
    prepare_agent_task_view(
        task_dir=task_dir,
        run_root=run_root,
        verifier_entrypoint="verifier.py",
    )
    (task_dir / "input.txt").write_text("changed", encoding="utf-8")

    with pytest.raises(ValueError, match="task input view does not match source"):
        prepare_agent_task_view(
            task_dir=task_dir,
            run_root=run_root,
            verifier_entrypoint="verifier.py",
        )


def test_agent_task_view_refuses_hardlink_alias_of_verifier(tmp_path: Path) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    verifier = task_dir / "verifier.py"
    verifier.write_text("private grader", encoding="utf-8")
    (task_dir / "challenge-data.txt").hardlink_to(verifier)

    with pytest.raises(ValueError, match="hard-linked task input"):
        prepare_agent_task_view(
            task_dir=task_dir,
            run_root=tmp_path / "run",
            verifier_entrypoint="verifier.py",
        )


@pytest.mark.parametrize("symlink_parent", [False, True])
def test_agent_task_view_rejects_symlinked_verifier_path_before_allocating(
    tmp_path: Path, symlink_parent: bool
) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    verifier_target = task_dir / "grader.py"
    verifier_target.write_text("private grader", encoding="utf-8")
    if symlink_parent:
        real_dir = task_dir / "real"
        real_dir.mkdir()
        (real_dir / "verifier.py").write_text("private grader", encoding="utf-8")
        (task_dir / "linked").symlink_to(real_dir, target_is_directory=True)
        entrypoint = "linked/verifier.py"
    else:
        (task_dir / "verifier.py").symlink_to(verifier_target)
        entrypoint = "verifier.py"

    with pytest.raises(ValueError, match="verifier entrypoint cannot contain symlinks"):
        validate_agent_task_inputs(task_dir=task_dir, verifier_entrypoint=entrypoint)

    run_root = tmp_path / "run"
    with pytest.raises(ValueError, match="verifier entrypoint cannot contain symlinks"):
        prepare_agent_task_view(
            task_dir=task_dir, run_root=run_root, verifier_entrypoint=entrypoint
        )
    assert not run_root.exists()
