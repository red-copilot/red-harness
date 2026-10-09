"""Linux process-group supervisor for trusted host CLI Agents.

The supervisor is the only process launched by the Harness. It receives a
parent-death signal and terminates the Agent's process group before exiting,
so a forcibly killed Orchestrator does not strand a long-running child.
"""
from __future__ import annotations

import ctypes
import os
import signal
import subprocess
import sys
import time


def _set_parent_death_signal() -> None:
    libc = ctypes.CDLL(None, use_errno=True)
    libc.prctl.argtypes = [
        ctypes.c_int,
        ctypes.c_ulong,
        ctypes.c_ulong,
        ctypes.c_ulong,
        ctypes.c_ulong,
    ]
    libc.prctl.restype = ctypes.c_int
    if libc.prctl(1, signal.SIGTERM, 0, 0, 0) != 0:  # PR_SET_PDEATHSIG
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def _signal_group(pgid: int, sig: int) -> bool:
    try:
        os.killpg(pgid, sig)
    except ProcessLookupError:
        return False
    return True


def _terminate_group(child: subprocess.Popen[bytes], *, grace: float = 1.0) -> None:
    pgid = child.pid
    _signal_group(pgid, signal.SIGTERM)
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        child.poll()
        try:
            os.killpg(pgid, 0)
        except ProcessLookupError:
            break
        time.sleep(0.02)
    else:
        _signal_group(pgid, signal.SIGKILL)
    try:
        child.wait(timeout=2)
    except subprocess.TimeoutExpired:
        _signal_group(pgid, signal.SIGKILL)
        child.wait(timeout=2)


def main() -> int:
    if sys.platform != "linux":
        raise RuntimeError("process_guard requires Linux")
    arguments = sys.argv[1:]
    if arguments and arguments[0] == "--":
        arguments = arguments[1:]
    if not arguments:
        return 2

    parent_pid = os.getppid()
    parent_died = False
    child: subprocess.Popen[bytes] | None = None

    def handle_parent_signal(_signal_number: int, _frame: object) -> None:
        nonlocal parent_died
        parent_died = True

    signal.signal(signal.SIGTERM, handle_parent_signal)
    _set_parent_death_signal()
    if os.getppid() != parent_pid:
        parent_died = True

    # Prevent a parent-death signal from arriving after fork but before the
    # child process group is recorded by this supervisor.
    previous_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM})
    try:
        child = subprocess.Popen(arguments, start_new_session=True)
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous_mask)

    try:
        while child.poll() is None:
            if parent_died:
                _terminate_group(child)
                return 128 + signal.SIGTERM
            time.sleep(0.02)
        return_code = child.returncode or 0
        # The Agent may have left subprocesses behind after exiting. They are
        # part of its private session and must not outlive the run.
        _terminate_group(child, grace=0.1)
        if parent_died:
            return 128 + signal.SIGTERM
        if return_code < 0:
            return 128 + abs(return_code)
        return return_code
    finally:
        if parent_died and child.poll() is None:
            _terminate_group(child)


if __name__ == "__main__":
    raise SystemExit(main())
