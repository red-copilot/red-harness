from __future__ import annotations

import os
import subprocess
import sys
import time
from pathlib import Path

import pytest


@pytest.mark.skipif(sys.platform != "linux", reason="process group cleanup requires Linux")
def test_process_guard_cleans_agent_descendants_on_normal_exit(tmp_path: Path) -> None:
    guard = Path(__file__).resolve().parents[1] / "src/harness/process_guard.py"
    pid_file = tmp_path / "grandchild.pid"
    agent = (
        "import pathlib,subprocess,sys;"
        "child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);"
        f"pathlib.Path({str(pid_file)!r}).write_text(str(child.pid))"
    )

    result = subprocess.run(
        [sys.executable, str(guard), "--", sys.executable, "-c", agent],
        check=False,
        timeout=10,
    )

    assert result.returncode == 0
    grandchild_pid = int(pid_file.read_text(encoding="utf-8"))
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            os.kill(grandchild_pid, 0)
        except ProcessLookupError:
            break
        time.sleep(0.05)
    else:
        pytest.fail("Agent descendant remained alive after the Agent exited")
