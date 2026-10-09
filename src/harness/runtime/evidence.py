from __future__ import annotations

import hashlib
import json
import math
import os
import stat
from datetime import UTC, datetime
from pathlib import Path

from ..progress import ProgressLedger
from ..trace import TraceRecorder
from ..verification import (
    AuthorizedObjectiveVerdict,
    EvidenceRef,
    ObjectiveStatus,
    ObjectiveVerdict,
    VerifierAuthority,
    VerifierRegistry,
)


def persist_objective_verdict(
    *,
    run_dir: Path,
    run_id: str,
    world_revision: int,
    producer_authority: VerifierAuthority,
    registry: VerifierRegistry,
    progress: ProgressLedger,
    trace: TraceRecorder,
    success: bool | None = None,
    score: float = 0.0,
    milestones: dict[str, bool] | None = None,
    status: ObjectiveStatus | None = None,
    failure_type: str | None = None,
) -> AuthorizedObjectiveVerdict:
    """Persist a redacted evaluator result and record its authorized verdict."""
    producer = producer_authority.producer
    if producer not in {"task_verifier", "benchmark_evaluator"}:
        raise ValueError("objective verdict requires a task or benchmark evaluator authority")
    if not math.isfinite(score):
        raise ValueError("objective score must be finite")
    captured_at = datetime.now(UTC)
    if status is None:
        if success is None:
            raise ValueError("objective outcome requires success or status")
        status = "verified" if success else "contradicted"
    elif success is not None and success != (status == "verified"):
        raise ValueError("objective success and status disagree")
    if status == "unavailable" and (
        not isinstance(failure_type, str)
        or not failure_type
        or len(failure_type) > 128
        or not failure_type.replace("_", "").isalnum()
    ):
        raise ValueError("unavailable objective outcome requires a safe failure type")
    if status != "unavailable" and failure_type is not None:
        raise ValueError("failure type is only valid for unavailable outcomes")
    action_id = f"objective-evaluation:{run_id}"
    artifact_payload = {
        "schema_version": "harness/objective-evidence/v1",
        "run_id": run_id,
        "action_id": action_id,
        "producer": producer,
        "captured_at": captured_at.isoformat(),
        "world_revision": world_revision,
        "status": status,
        "score": float(score),
        "milestones": dict(sorted((milestones or {}).items())),
    }
    if failure_type is not None:
        artifact_payload["failure_type"] = failure_type
    artifact_bytes = (
        json.dumps(artifact_payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        + "\n"
    ).encode("utf-8")
    if len(artifact_bytes) > 1024 * 1024:
        raise ValueError("objective evidence exceeds the 1 MiB artifact limit")
    digest = hashlib.sha256(artifact_bytes).hexdigest()
    relative_ref = Path("evidence") / "sha256" / f"{digest}.json"
    directory_fd = _open_evidence_directory(run_dir)
    filename = f"{digest}.json"
    try:
        try:
            fd = os.open(
                filename,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
                0o600,
                dir_fd=directory_fd,
            )
        except FileExistsError:
            fd = os.open(
                filename,
                os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
                dir_fd=directory_fd,
            )
            try:
                existing_stat = os.fstat(fd)
                if not stat.S_ISREG(existing_stat.st_mode) or existing_stat.st_size > 1024 * 1024:
                    raise RuntimeError("unsafe content-addressed objective evidence artifact")
                with os.fdopen(fd, "rb", closefd=False) as handle:
                    existing_bytes = handle.read()
                if existing_bytes != artifact_bytes:
                    raise RuntimeError("content-addressed objective evidence collision")
            finally:
                os.close(fd)
        else:
            try:
                with os.fdopen(fd, "wb", closefd=True) as handle:
                    handle.write(artifact_bytes)
                    handle.flush()
                    os.fsync(handle.fileno())
            except Exception:
                try:
                    os.unlink(filename, dir_fd=directory_fd)
                except OSError:
                    pass
                raise
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)

    evidence = EvidenceRef(
        evidence_id=f"sha256:{digest}",
        run_id=run_id,
        action_id=action_id,
        producer=producer,
        captured_at=captured_at,
        world_revision=world_revision,
        artifact_ref=relative_ref.as_posix(),
        artifact_hash=digest,
    )
    verdict = ObjectiveVerdict(
        run_id=run_id,
        producer=producer,
        captured_at=captured_at,
        world_revision=world_revision,
        status=status,
        evidence=(evidence,),
    )
    authorized = registry.authorize_objective(producer_authority, verdict)
    progress.record_objective_verdict(authorized, registry)
    trace.emit(
        "objective.verdict",
        actor="harness",
        data=verdict.model_dump(mode="json"),
    )
    return authorized


def _open_evidence_directory(run_dir: Path) -> int:
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    current_fd = os.open(run_dir, flags)
    try:
        for component in ("evidence", "sha256"):
            try:
                os.mkdir(component, mode=0o700, dir_fd=current_fd)
            except FileExistsError:
                pass
            next_fd = os.open(component, flags, dir_fd=current_fd)
            os.close(current_fd)
            current_fd = next_fd
        return current_fd
    except Exception:
        os.close(current_fd)
        raise
