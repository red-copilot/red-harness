import json
from pathlib import Path
from unittest.mock import Mock

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
