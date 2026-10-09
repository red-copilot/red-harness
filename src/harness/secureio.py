"""Small no-follow helpers for files written by untrusted Agent processes."""

from __future__ import annotations

import os
import stat
from pathlib import Path
from typing import IO

_DEFAULT_READ_LIMIT = 64 * 1024 * 1024


def open_regular_file(
    path: Path,
    mode: str,
    *,
    encoding: str | None = "utf-8",
    errors: str = "strict",
) -> IO[str]:
    if not hasattr(os, "O_NOFOLLOW"):
        raise OSError("platform does not support no-follow Agent file access")
    flags = {
        "r": os.O_RDONLY,
        "a": os.O_WRONLY | os.O_APPEND | os.O_CREAT,
        "w": os.O_WRONLY | os.O_TRUNC | os.O_CREAT,
        "rb": os.O_RDONLY,
        "ab": os.O_WRONLY | os.O_APPEND | os.O_CREAT,
        "wb": os.O_WRONLY | os.O_TRUNC | os.O_CREAT,
    }[mode]
    flags |= os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
    if mode in {"r", "rb"}:
        flags |= getattr(os, "O_NONBLOCK", 0)
    descriptor = os.open(path, flags, 0o600)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise OSError(f"expected a regular file: {path}")
        if "b" in mode:
            return os.fdopen(descriptor, mode)  # type: ignore[return-value]
        return os.fdopen(descriptor, mode, encoding=encoding, errors=errors)
    except BaseException:
        os.close(descriptor)
        raise


def read_regular_text(
    path: Path,
    *,
    encoding: str = "utf-8",
    errors: str = "replace",
    max_bytes: int = _DEFAULT_READ_LIMIT,
) -> str:
    with open_regular_file(path, "rb") as handle:
        data = handle.read(max_bytes + 1)  # type: ignore[union-attr]
    if len(data) > max_bytes:
        raise OSError(f"Agent output exceeds the {max_bytes}-byte read limit: {path}")
    return data.decode(encoding, errors=errors)


def open_beneath(
    root: Path,
    relative_path: Path,
    mode: str,
    *,
    create_parents: bool = False,
) -> IO[str]:
    """Open an Agent workspace file without following any path component symlink."""
    if not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_DIRECTORY"):
        raise OSError("platform does not support safe workspace path traversal")
    if relative_path.is_absolute() or not relative_path.parts:
        raise OSError("workspace path must be relative and name a file")
    parts = relative_path.parts
    if any(part in {".", ".."} for part in parts):
        raise OSError("workspace path traversal is not allowed")
    directory_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
    directory_fd = os.open(root, directory_flags)
    try:
        for part in parts[:-1]:
            try:
                child_fd = os.open(part, directory_flags, dir_fd=directory_fd)
            except FileNotFoundError:
                if not create_parents:
                    raise
                os.mkdir(part, mode=0o700, dir_fd=directory_fd)
                child_fd = os.open(part, directory_flags, dir_fd=directory_fd)
            os.close(directory_fd)
            directory_fd = child_fd

        flags_by_mode = {
            "r": os.O_RDONLY,
            "rb": os.O_RDONLY,
            "a": os.O_WRONLY | os.O_APPEND | os.O_CREAT,
            "ab": os.O_WRONLY | os.O_APPEND | os.O_CREAT,
            "w": os.O_WRONLY | os.O_TRUNC | os.O_CREAT,
            "wb": os.O_WRONLY | os.O_TRUNC | os.O_CREAT,
        }
        flags = flags_by_mode[mode] | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
        if mode in {"r", "rb"}:
            flags |= getattr(os, "O_NONBLOCK", 0)
        descriptor = os.open(parts[-1], flags, 0o600, dir_fd=directory_fd)
        try:
            if not stat.S_ISREG(os.fstat(descriptor).st_mode):
                raise OSError(f"expected a regular workspace file: {relative_path}")
            if "b" in mode:
                return os.fdopen(descriptor, mode)  # type: ignore[return-value]
            return os.fdopen(descriptor, mode, encoding="utf-8")
        except BaseException:
            os.close(descriptor)
            raise
    finally:
        os.close(directory_fd)
