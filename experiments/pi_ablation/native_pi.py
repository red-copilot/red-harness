"""Native Pi/Kali benchmark baseline: no Harness solver, world or planner.

Run explicitly with `python -m experiments.pi_ablation.native_pi ...`.
Only use on targets for which you have competition authorization.
"""
from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import subprocess
import time
import uuid
from pathlib import Path

from harness.benchmark.tsec_adapter import TSecBenchmarkAdapter
from harness.benchmark.tsec_client import TSecClientAdapter, load_tsec_config
from harness.benchmark.tsec import extract_flags
from harness.models import load_agent
from harness.benchmark.base import Submission


def command_for_pi(spec, prompt: str, workspace: Path, name: str) -> list[str]:
    """Generate a standalone Pi invocation; deliberately avoid PiAdapter._prompt_text."""
    if spec.type != "pi" or spec.network != "host" or spec.network_profile != "benchmark-only":
        raise ValueError("native TSec baseline requires Pi, host networking, benchmark-only profile")
    pi = spec.pi
    if pi is None or pi.mode != "json":
        raise ValueError("native baseline requires pi.mode: json (one-shot)")
    if pi.extensions or pi.context_files or pi.skills or pi.mcp or pi.prompt_templates:
        raise ValueError("disable extensions/context/skills/mcp/prompt templates for baseline")
    if any(key.startswith("BENCHMARK_") for key in (*spec.env.keys(), *pi.env_passthrough)):
        raise ValueError("benchmark control-plane credentials must not enter Pi")
    cmd = ["docker", "run", "--rm", "--pull=never", "--name", name,
           "--network", "host", "--read-only", "--cap-drop", "ALL",
           "--security-opt", "no-new-privileges", "--pids-limit", "512",
           "--memory", "4g", "--cpus", "4",
           "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m",
           "--tmpfs", "/var/tmp:rw,nosuid,nodev,size=256m",
           "-v", f"{workspace.resolve()}:/workspace:rw", "-w", "/workspace",
           "-e", "HOME=/workspace", "-e", "PI_CODING_AGENT_DIR=/workspace/pi-agent",
           "-e", "PI_CODING_AGENT_SESSION_DIR=/workspace/pi-sessions",
           "-e", "PI_SKIP_VERSION_CHECK=1", "-e", "PI_TELEMETRY=0", "-e", "PI_OFFLINE=1"]
    for cap in pi.cap_add:
        cmd.extend(["--cap-add", cap])
    for key in pi.env_passthrough:
        if key not in os.environ:
            raise ValueError(f"missing Pi provider credential: {key}")
        cmd.extend(["-e", key])  # Docker reads host environment; no credential in argv.
    for key, value in spec.env.items():
        if key.upper().endswith(("TOKEN", "SECRET", "PASSWORD", "KEY")):
            raise ValueError("do not configure credentials via agent.env")
        cmd.extend(["-e", f"{key}={value}"])
    cmd += [str(spec.image), pi.binary, *pi.launcher_args, "--mode", "json",
            "--no-session", "--no-approve", "--no-context-files",
            "--no-extensions", "--no-skills", "--no-prompt-templates",
            "--no-themes", "--no-mcp"]
    if pi.tools:
        cmd += ["--tools", ",".join(pi.tools)]
    else:
        cmd += ["--no-tools"]
    if pi.provider:
        cmd += ["--provider", pi.provider]
    cmd += ["--model", pi.model]
    if pi.thinking:
        cmd += ["--thinking", pi.thinking]
    cmd += [prompt]
    return cmd


def native_prompt(session) -> str:
    """No Harness protocol, world-state, feedback inbox or replan instructions."""
    description = session.objective.description.split("\n\nInvestigate only the benchmark targets")[0]
    targets = "\n".join(f"- {target.address}" for target in session.targets)
    return (
        "Solve this authorized benchmark using only the listed targets and local tools. "
        "Work independently; no external controller will steer your reasoning. "
        "When you discover a candidate flag, include exactly HARNESS_FLAG=<flag> "
        "in a final assistant message. You may report multiple candidates.\n\n"
        + description + "\n\nAuthorized targets:\n" + targets
    )


def run_pi(cmd: list[str], *, seconds: int, workspace: Path, name: str) -> tuple[int | None, bool, float]:
    started = time.monotonic()
    log = workspace / "pi.jsonl"
    timed_out = False
    code = None
    with log.open("wb") as out:
        process = subprocess.Popen(cmd, stdout=out, stderr=subprocess.DEVNULL)
        try:
            code = process.wait(timeout=seconds)
        except subprocess.TimeoutExpired:
            timed_out = True
            subprocess.run(["docker", "kill", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, check=False)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    return code, timed_out, time.monotonic() - started


async def trial(config, *, spec, case_id: str, root: Path, wall_time: int, seed: int) -> dict:
    async with TSecClientAdapter(config) as client:
        adapter = TSecBenchmarkAdapter(client, use_hint=False)
        cases = await adapter.discover()
        matching = [case for case in cases if case.id == case_id]
        if len(matching) != 1:
            raise ValueError("challenge not found")
        case = matching[0]
        if case.metadata.get("is_completed") or case.metadata.get("correct_flag_count"):
            raise ValueError("primary baseline requires fresh, unsolved challenge")
        session = await adapter.provision(case)
        name = "harness_baseline_" + uuid.uuid4().hex[:12]
        workspace = root / name
        workspace.mkdir(parents=True, exist_ok=False)
        started = time.monotonic()
        result = None
        try:
            cmd = command_for_pi(spec, native_prompt(session), workspace, name)
            code, timed_out, elapsed = await asyncio.to_thread(
                run_pi, cmd, seconds=wall_time, workspace=workspace, name=name)
            raw = (workspace / "pi.jsonl").read_text(encoding="utf-8", errors="replace")
            flags = extract_flags(raw)
            # Extract only structured Pi assistant messages, never tool outputs.
            accepted = 0
            awarded = 0.0
            hashes = []
            for flag in flags:
                response = await adapter.submit(session, Submission(type="flag", value=flag))
                hashes.append(hashlib.sha256(flag.encode()).hexdigest())
                accepted += int(response.accepted)
                awarded += response.score_delta
            verified = await adapter.evaluate(session)
            result = {
                "arm": "01-native-pi", "challenge_id": case_id, "seed": seed,
                "status": "completed" if verified.success else "timeout" if timed_out else "unsolved",
                "verified_success": verified.success,
                "verified_flags": int(verified.metadata.get("correct", 0)),
                "awarded_score": awarded,
                "elapsed_seconds": elapsed, "input_tokens": None, "output_tokens": None,
                "model_calls": None, "tool_calls": None, "cost_usd": None,
                "first_flag_seconds": None, "hint_used": False,
                "initial_correct": int(session.metadata["initial_correct"]),
                "exit_code": code, "candidate_hashes": hashes, "accepted_candidates": accepted,
                "model": spec.pi.model, "image": spec.image, "wall_time_limit": wall_time,
                "note": "No live feedback, world state, planner or cooperation; token accounting pending."
            }
        finally:
            await adapter.teardown(session)
        assert result is not None
        (workspace / "result.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
        return result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path, default=Path("agents/examples/pi-tsec.yaml"))
    parser.add_argument("--challenge", required=True)
    parser.add_argument("--runs-root", type=Path, default=Path(".harness/native-pi"))
    parser.add_argument("--wall-time", type=int, default=3600)
    parser.add_argument("--seed", type=int, default=101)
    args = parser.parse_args()
    if not 1 <= args.wall_time <= 3600:
        parser.error("wall-time must be 1..3600")
    spec = load_agent(args.agent)
    args.runs_root.mkdir(parents=True, exist_ok=True)
    outcome = asyncio.run(trial(load_tsec_config(), spec=spec,
                                case_id=args.challenge, root=args.runs_root,
                                wall_time=args.wall_time, seed=args.seed))
    print(json.dumps(outcome, indent=2))


if __name__ == "__main__":
    main()
