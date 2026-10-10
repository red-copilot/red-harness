"""Writable, untrusted workspace exposed to one Agent execution."""

from __future__ import annotations

import hashlib
import os
import shutil
import stat
import tempfile
from pathlib import Path

from .secureio import open_beneath

_HARNESS_VIEWS = ("world.context.txt", "progress.json")
_CONTAINER_UID = 65532
_CONTAINER_GID = 65532


def create_agent_workspace(run_dir: Path) -> Path:
    workspace = run_dir / "agent-workspace"
    workspace.mkdir(mode=0o700)
    return workspace


def validate_agent_task_inputs(*, task_dir: Path, verifier_entrypoint: str) -> None:
    """Validate the bounded task tree that container Agents will receive."""
    source_arg = Path(task_dir)
    if source_arg.is_symlink():
        raise ValueError("task directory cannot be a symlink")
    try:
        source = source_arg.resolve(strict=True)
    except OSError as exc:
        raise ValueError("task directory is unavailable") from exc
    if not source.is_dir():
        raise ValueError("task directory must be a real directory")
    entrypoint = Path(verifier_entrypoint)
    if entrypoint.is_absolute() or ".." in entrypoint.parts:
        raise ValueError("verifier entrypoint must be task-relative")
    _reject_symlinked_verifier_path(source, entrypoint)
    _tree_manifest(source)


def _reject_symlinked_verifier_path(root: Path, entrypoint: Path) -> None:
    """Reject verifier paths that traverse a symlink, including parent aliases."""
    current = root
    for index, part in enumerate(entrypoint.parts):
        current = current / part
        try:
            info = current.lstat()
        except FileNotFoundError:
            return
        except OSError as exc:
            raise ValueError("verifier entrypoint is unavailable") from exc
        if stat.S_ISLNK(info.st_mode):
            raise ValueError("verifier entrypoint cannot contain symlinks")
        if index < len(entrypoint.parts) - 1 and not stat.S_ISDIR(info.st_mode):
            return


def _tree_manifest(
    root: Path, *, exclude_subtree: Path | None = None
) -> dict[str, tuple[int, str]]:
    manifest: dict[str, tuple[int, str]] = {}
    total_bytes = 0
    for parent, directories, files in os.walk(root, followlinks=False):
        parent_path = Path(parent)
        retained: list[str] = []
        for name in directories:
            child = parent_path / name
            if child.is_symlink():
                continue
            relative = child.relative_to(root)
            if exclude_subtree is not None and (
                relative == exclude_subtree or exclude_subtree in relative.parents
            ):
                continue
            retained.append(name)
        directories[:] = sorted(retained)
        for name in sorted(files):
            path = parent_path / name
            try:
                info = path.stat(follow_symlinks=False)
                if not stat.S_ISREG(info.st_mode):
                    continue
                if info.st_nlink != 1:
                    raise ValueError("hard-linked task input is not safe to expose")
                digest = hashlib.sha256()
                with open_beneath(root, path.relative_to(root), "rb") as stream:
                    for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                        digest.update(chunk)
                after = path.stat(follow_symlinks=False)
                if (info.st_ino, info.st_size, info.st_mtime_ns) != (
                    after.st_ino, after.st_size, after.st_mtime_ns
                ):
                    raise ValueError("task input changed while hashing")
            except OSError as exc:
                raise ValueError("task input view contains an unsafe file") from exc
            manifest[path.relative_to(root).as_posix()] = (info.st_size, digest.hexdigest())
            total_bytes += info.st_size
            if len(manifest) > 4096:
                raise ValueError("task input view exceeds file-count limit")
            if total_bytes > 2 * 1024 * 1024 * 1024:
                raise ValueError("task input view exceeds size limit")
    return manifest


def prepare_agent_task_view(
    *, task_dir: Path, run_root: Path, verifier_entrypoint: str
) -> Path:
    """Create a stable read-only task copy that omits the verifier program."""
    source_arg = Path(task_dir)
    if source_arg.is_symlink():
        raise ValueError("task directory cannot be a symlink")
    source = source_arg.resolve(strict=True)
    if not source.is_dir():
        raise ValueError("task directory must be a real directory")
    entrypoint = Path(verifier_entrypoint)
    if entrypoint.is_absolute() or ".." in entrypoint.parts:
        raise ValueError("verifier entrypoint must be task-relative")
    _reject_symlinked_verifier_path(source, entrypoint)
    excluded = entrypoint.as_posix()
    run_root.mkdir(parents=True, exist_ok=True, mode=0o700)
    if run_root.is_symlink() or not run_root.is_dir():
        raise ValueError("run root must be a real directory")
    resolved_run_root = run_root.resolve()
    if resolved_run_root == source or source.is_relative_to(resolved_run_root):
        raise ValueError("task directory cannot contain authoritative run state")
    exclude_subtree = (
        resolved_run_root.relative_to(source)
        if resolved_run_root.is_relative_to(source)
        else None
    )
    destination = run_root / "agent-task-input"
    if destination.is_symlink():
        raise ValueError("Agent task input view cannot be a symlink")

    source_manifest = _tree_manifest(source, exclude_subtree=exclude_subtree)
    source_manifest.pop(excluded, None)
    if destination.exists():
        if not destination.is_dir() or _tree_manifest(destination) != source_manifest:
            raise ValueError("task input view does not match source")
        return destination

    stage = Path(tempfile.mkdtemp(prefix=".agent-task-input-", dir=run_root))
    total_bytes = 0
    try:
        for relative, (expected_size, expected_hash) in sorted(source_manifest.items()):
            source_file = source / relative
            target_file = stage / relative
            target_file.parent.mkdir(parents=True, exist_ok=True)
            digest = hashlib.sha256()
            copied = 0
            before = source_file.stat(follow_symlinks=False)
            with (
                open_beneath(source, Path(relative), "rb") as input_stream,
                target_file.open("xb") as output_stream,
            ):
                for chunk in iter(lambda: input_stream.read(1024 * 1024), b""):
                    copied += len(chunk)
                    total_bytes += len(chunk)
                    if total_bytes > 2 * 1024 * 1024 * 1024:
                        raise ValueError("task input view exceeds size limit")
                    digest.update(chunk)
                    output_stream.write(chunk)
            after = source_file.stat(follow_symlinks=False)
            if (before.st_ino, before.st_size, before.st_mtime_ns) != (
                after.st_ino, after.st_size, after.st_mtime_ns
            ) or copied != expected_size or digest.hexdigest() != expected_hash:
                raise ValueError("task input changed while copying")
        for parent, directories, files in os.walk(stage, topdown=False):
            parent_path = Path(parent)
            for name in files:
                (parent_path / name).chmod(0o444)
            for name in directories:
                (parent_path / name).chmod(0o555)
            parent_path.chmod(0o555)
        os.replace(stage, destination)
    except BaseException:
        shutil.rmtree(stage, ignore_errors=True)
        raise
    if _tree_manifest(destination) != source_manifest:
        raise ValueError("task input view does not match source")
    return destination


def prepare_agent_workspace_for_container(workspace: Path) -> tuple[int, int]:
    """Make the workspace writable by a dedicated, non-root container identity."""
    info = workspace.stat(follow_symlinks=False)
    if not stat.S_ISDIR(info.st_mode):
        raise ValueError("agent workspace must be a real directory")
    uid, gid = (
        (_CONTAINER_UID, _CONTAINER_GID)
        if os.geteuid() == 0
        else (os.geteuid(), os.getegid())
    )
    for parent, directories, files in os.walk(workspace, followlinks=False):
        parent_path = Path(parent)
        for path in (parent_path / name for name in directories + files):
            path_info = path.lstat()
            if stat.S_ISLNK(path_info.st_mode):
                continue
            os.chown(path, uid, gid, follow_symlinks=False)
            if stat.S_ISDIR(path_info.st_mode):
                os.chmod(path, 0o700, follow_symlinks=False)
            elif stat.S_ISREG(path_info.st_mode):
                os.chmod(path, stat.S_IMODE(path_info.st_mode) | 0o600, follow_symlinks=False)
        os.chown(parent_path, uid, gid, follow_symlinks=False)
        os.chmod(parent_path, 0o700, follow_symlinks=False)
    return uid, gid


def sync_agent_workspace(*, run_dir: Path, workspace: Path) -> None:
    """Refresh Agent-readable state with atomic replacement of untrusted files."""
    for name in _HARNESS_VIEWS:
        source = run_dir / name
        if not source.is_file():
            continue
        # Multiple solver checkpoints can request the same projection. Avoid
        # replacing an unchanged view; never follow workspace symlinks.
        payload = source.read_bytes()
        target = workspace / name
        try:
            fd = os.open(target, os.O_RDONLY | os.O_NOFOLLOW)
        except OSError:
            pass
        else:
            try:
                info = os.fstat(fd)
                if stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_size == len(payload):
                    with os.fdopen(os.dup(fd), "rb") as existing:
                        if existing.read() == payload:
                            continue
            finally:
                os.close(fd)
        descriptor, temp_name = tempfile.mkstemp(prefix=f".{name}.", dir=workspace)
        try:
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            workspace_info = workspace.stat(follow_symlinks=False)
            os.chown(temp_name, workspace_info.st_uid, workspace_info.st_gid)
            os.chmod(temp_name, 0o644)
            os.replace(temp_name, workspace / name)
        finally:
            if os.path.exists(temp_name):
                os.unlink(temp_name)
