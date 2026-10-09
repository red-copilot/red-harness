import errno
import json
import os
from pathlib import Path
from unittest.mock import Mock

import pytest

from harness.runtime.results import persist_run_result


def test_persist_run_result_preserves_json_and_terminal_event(tmp_path: Path) -> None:
    trace = Mock()
    result = {
        "status": "finished",
        "success": True,
        "score": 1.0,
        "metrics": {"duration_ms": 42},
        "message": "验证完成",
    }
    persist_run_result(run_dir=tmp_path, trace=trace, result=result)
    path = tmp_path / "result.json"
    assert path.read_text(encoding="utf-8").endswith("\n")
    assert json.loads(path.read_text(encoding="utf-8")) == result
    trace.emit.assert_called_once_with(
        "run.finished",
        data={
            "status": "finished",
            "success": True,
            "score": 1.0,
            "metrics": {"duration_ms": 42},
        },
    )


def test_persist_run_result_does_not_mutate_result(tmp_path: Path) -> None:
    result = {
        "status": "error",
        "success": False,
        "score": 0.0,
        "metrics": {},
        "additional": {"hello": "world"},
    }
    original = json.loads(json.dumps(result))
    persist_run_result(run_dir=tmp_path, trace=Mock(), result=result)
    assert result == original


def test_persist_run_result_is_idempotent_for_same_terminal_result(tmp_path: Path) -> None:
    trace = Mock()
    result = {"status": "finished", "success": True, "score": 1.0, "metrics": {}}

    persist_run_result(run_dir=tmp_path, trace=trace, result=result)
    persist_run_result(run_dir=tmp_path, trace=trace, result=result)

    trace.emit.assert_called_once()
    assert json.loads((tmp_path / "result.json").read_text(encoding="utf-8")) == result


def test_persist_run_result_refuses_to_overwrite_conflicting_terminal_result(
    tmp_path: Path,
) -> None:
    trace = Mock()
    first = {"status": "finished", "success": True, "score": 1.0, "metrics": {}}
    conflicting = {"status": "error", "success": False, "score": 0.0, "metrics": {}}
    persist_run_result(run_dir=tmp_path, trace=trace, result=first)

    with pytest.raises(RuntimeError, match="terminal result already exists"):
        persist_run_result(run_dir=tmp_path, trace=trace, result=conflicting)

    assert json.loads((tmp_path / "result.json").read_text(encoding="utf-8")) == first
    trace.emit.assert_called_once()


def test_result_disk_full_during_sync_leaves_no_partial_terminal_artifact(
    tmp_path: Path, monkeypatch
) -> None:
    first = {"status": "finished", "success": True, "score": 1.0, "metrics": {}}
    second = {"status": "failed", "success": False, "score": 0.0, "metrics": {}}
    persist_run_result(run_dir=tmp_path, trace=Mock(), result=first)
    original = (tmp_path / "result.json").read_bytes()

    def disk_full(_descriptor: int) -> None:
        raise OSError(errno.ENOSPC, "injected disk full")

    monkeypatch.setattr(os, "fsync", disk_full)
    with pytest.raises(OSError) as error:
        persist_run_result(run_dir=tmp_path, trace=Mock(), result=second)

    assert error.value.errno == errno.ENOSPC
    assert (tmp_path / "result.json").read_bytes() == original
    assert not list(tmp_path.glob(".result-*"))
