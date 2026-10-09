from __future__ import annotations

import json
import math
import sqlite3
import time
import uuid
from pathlib import Path
from statistics import median
from typing import Any

from pydantic import BaseModel, Field

MAX_TERMINAL_RESULT_BYTES = 1024 * 1024


def validate_terminal_result(result: dict[str, Any]) -> dict[str, Any]:
    """Validate result fields used by the durable queue and leaderboard."""
    if not isinstance(result, dict):
        raise TypeError("terminal result must be an object")
    try:
        encoded = json.dumps(
            result, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise ValueError("terminal result must be finite JSON") from exc
    if len(encoded) > MAX_TERMINAL_RESULT_BYTES:
        raise ValueError("terminal result exceeds the 1048576-byte limit")

    if "agent_id" in result and (
        not isinstance(result["agent_id"], str) or len(result["agent_id"]) > 256
    ):
        raise ValueError("terminal result agent_id must be a string of at most 256 characters")
    if "success" in result and not isinstance(result["success"], bool):
        raise ValueError("terminal result success must be a boolean")
    if "score" in result:
        score = result["score"]
        if not _finite_result_number(score):
            raise ValueError("terminal result score must be a finite number")
    if "metrics" in result:
        metrics = result["metrics"]
        if not isinstance(metrics, dict):
            raise ValueError("terminal result metrics must be an object")
        for name in ("duration_ms", "cost_usd"):
            if name not in metrics:
                continue
            value = metrics[name]
            if not _finite_result_number(value) or value < 0:
                raise ValueError(f"terminal result metric {name} must be finite and non-negative")
    return result


def _finite_result_number(value: Any) -> bool:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    try:
        return math.isfinite(float(value))
    except (OverflowError, ValueError):
        return False


class GatewayJobSpec(BaseModel):
    enabled: bool = False
    policy_path: str | None = Field(default=None, max_length=4096)
    mode: str = Field(default="host", pattern=r"^(host|sidecar)$")
    sidecar_image: str | None = Field(default=None, max_length=512)
    sidecar_runtime: str | None = Field(default=None, max_length=256)
    model_upstream: str | None = Field(default=None, max_length=4096)
    model_api_key_env: str = Field(default="HARNESS_MODEL_API_KEY", min_length=1, max_length=256)
    input_price_per_million_usd: float = Field(default=0.0, ge=0)
    output_price_per_million_usd: float = Field(default=0.0, ge=0)


class JobPayload(BaseModel):
    task_path: str = Field(min_length=1, max_length=4096)
    agent_path: str = Field(min_length=1, max_length=4096)
    runs_root: str = Field(default=".harness/runs", min_length=1, max_length=4096)
    allow_host_agent: bool = False
    seed: int = Field(default=0, ge=0)
    gateway: GatewayJobSpec = Field(default_factory=GatewayJobSpec)


class SQLiteQueue:
    """Reference queue backend with transactional leases.

    SQLite is intended for a single control-plane instance. Workers may be remote and
    communicate through the HTTP control plane; a future PostgreSQL backend can keep
    this interface unchanged.
    """

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._init_db()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout=30000")
        return connection

    @staticmethod
    def _enable_wal(db: sqlite3.Connection) -> None:
        deadline = time.monotonic() + 30
        while True:
            try:
                db.execute("PRAGMA journal_mode=WAL")
                return
            except sqlite3.OperationalError as exc:
                if "locked" not in str(exc).lower() or time.monotonic() >= deadline:
                    raise
                time.sleep(0.05)

    def _init_db(self) -> None:
        with self._connect() as db:
            self._enable_wal(db)
            # Serialize schema inspection and ALTER TABLE decisions so two
            # control-plane processes cannot apply the same migration at once.
            db.execute("BEGIN IMMEDIATE")
            db.execute(
                """
                CREATE TABLE IF NOT EXISTS jobs (
                    id TEXT PRIMARY KEY,
                    run_id TEXT,
                    state TEXT NOT NULL,
                    payload_json TEXT NOT NULL,
                    result_json TEXT,
                    error TEXT,
                    lease_owner TEXT,
                    lease_expires_at REAL,
                    terminal_owner TEXT,
                    attempts INTEGER NOT NULL DEFAULT 0,
                    created_at REAL NOT NULL,
                    updated_at REAL NOT NULL
                )
                """
            )
            db.execute(
                "CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(state, lease_expires_at, created_at)"
            )
            columns = {row["name"] for row in db.execute("PRAGMA table_info(jobs)")}
            if "run_id" not in columns:
                db.execute("ALTER TABLE jobs ADD COLUMN run_id TEXT")
            if "terminal_owner" not in columns:
                db.execute("ALTER TABLE jobs ADD COLUMN terminal_owner TEXT")
            db.execute(
                "UPDATE jobs SET run_id='run_' || substr(id, 5) WHERE run_id IS NULL"
            )

    @staticmethod
    def _validate_lease(worker_id: str, lease_seconds: int) -> None:
        if not worker_id or len(worker_id) > 200:
            raise ValueError("worker_id must contain 1 to 200 characters")
        if (
            isinstance(lease_seconds, bool)
            or not isinstance(lease_seconds, int)
            or not 10 <= lease_seconds <= 3600
        ):
            raise ValueError("lease_seconds must be between 10 and 3600")

    @staticmethod
    def _row(row: sqlite3.Row | None) -> dict[str, Any] | None:
        if row is None:
            return None
        return {
            "id": row["id"],
            "run_id": row["run_id"],
            "state": row["state"],
            "payload": json.loads(row["payload_json"]),
            "result": json.loads(row["result_json"]) if row["result_json"] else None,
            "error": row["error"],
            "lease_owner": row["lease_owner"],
            "lease_expires_at": row["lease_expires_at"],
            "terminal_owner": row["terminal_owner"],
            "attempts": row["attempts"],
            "created_at": row["created_at"],
            "updated_at": row["updated_at"],
        }

    def submit(self, payload: JobPayload) -> dict[str, Any]:
        now = time.time()
        job_id = f"job_{uuid.uuid4().hex}"
        run_id = f"run_{job_id.removeprefix('job_')}"
        with self._connect() as db:
            db.execute(
                """
                INSERT INTO jobs(id, run_id, state, payload_json, created_at, updated_at)
                VALUES (?, ?, 'queued', ?, ?, ?)
                """,
                (job_id, run_id, payload.model_dump_json(), now, now),
            )
        return self.get(job_id)  # type: ignore[return-value]

    def get(self, job_id: str) -> dict[str, Any] | None:
        with self._connect() as db:
            row = db.execute("SELECT * FROM jobs WHERE id = ?", (job_id,)).fetchone()
        return self._row(row)

    def list(self, *, limit: int = 100) -> list[dict[str, Any]]:
        with self._connect() as db:
            rows = db.execute(
                "SELECT * FROM jobs ORDER BY created_at DESC LIMIT ?", (limit,)
            ).fetchall()
        return [self._row(row) for row in rows if row is not None]  # type: ignore[misc]

    def claim(self, worker_id: str, *, lease_seconds: int = 60) -> dict[str, Any] | None:
        self._validate_lease(worker_id, lease_seconds)
        now = time.time()
        expires = now + lease_seconds
        with self._connect() as db:
            db.execute("BEGIN IMMEDIATE")
            db.execute(
                """
                UPDATE jobs
                SET state='reconciliation_required', lease_owner=NULL,
                    lease_expires_at=NULL, updated_at=?,
                    error=COALESCE(error, 'worker lease expired; reconcile run before retry')
                WHERE state='running' AND lease_expires_at IS NOT NULL
                    AND lease_expires_at <= ?
                """,
                (now, now),
            )
            row = db.execute(
                """
                SELECT * FROM jobs
                WHERE state = 'queued'
                ORDER BY created_at
                LIMIT 1
                """
            ).fetchone()
            if row is None:
                db.execute("COMMIT")
                return None
            db.execute(
                """
                UPDATE jobs
                SET state='running', lease_owner=?, lease_expires_at=?,
                    attempts=attempts+1, updated_at=?, error=NULL
                WHERE id=?
                """,
                (worker_id, expires, now, row["id"]),
            )
            db.execute("COMMIT")
        return self.get(str(row["id"]))

    def requeue(self, job_id: str) -> bool:
        """Explicitly retry an expired job after an operator reconciles its run."""
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET state='queued', error=NULL, updated_at=?
                WHERE id=? AND state='reconciliation_required'
                    AND lease_owner IS NULL AND lease_expires_at IS NULL
                """,
                (now, job_id),
            )
        return cursor.rowcount == 1

    def heartbeat(self, job_id: str, worker_id: str, *, lease_seconds: int = 60) -> bool:
        self._validate_lease(worker_id, lease_seconds)
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET lease_expires_at=?, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=? AND lease_expires_at>?
                """,
                (now + lease_seconds, now, job_id, worker_id, now),
            )
        return cursor.rowcount == 1

    def complete(self, job_id: str, worker_id: str, result: dict[str, Any]) -> bool:
        validate_terminal_result(result)
        now = time.time()
        result_json = json.dumps(
            result,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET state='completed', result_json=?, error=NULL,
                    lease_owner=NULL, lease_expires_at=NULL, terminal_owner=?, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=? AND lease_expires_at>?
                """,
                (result_json, worker_id, now, job_id, worker_id, now),
            )
            if cursor.rowcount == 1:
                return True
            row = db.execute(
                "SELECT state, terminal_owner, result_json FROM jobs WHERE id=?",
                (job_id,),
            ).fetchone()
        return bool(
            row is not None
            and row["state"] == "completed"
            and row["terminal_owner"] == worker_id
            and row["result_json"] == result_json
        )

    def fail(self, job_id: str, worker_id: str, error: str) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET state='failed', error=?, lease_owner=NULL,
                    lease_expires_at=NULL, terminal_owner=?, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=? AND lease_expires_at>?
                """,
                (error[:4000], worker_id, now, job_id, worker_id, now),
            )
            if cursor.rowcount == 1:
                return True
            row = db.execute(
                "SELECT state, terminal_owner, error FROM jobs WHERE id=?",
                (job_id,),
            ).fetchone()
        return bool(
            row is not None
            and row["state"] == "failed"
            and row["terminal_owner"] == worker_id
            and row["error"] == error[:4000]
        )

    def leaderboard(self) -> list[dict[str, Any]]:
        with self._connect() as db:
            rows = db.execute(
                "SELECT result_json FROM jobs WHERE state='completed' AND result_json IS NOT NULL"
            ).fetchall()

        groups: dict[str, list[dict[str, Any]]] = {}
        for row in rows:
            try:
                result = validate_terminal_result(json.loads(row["result_json"]))
            except (TypeError, ValueError, json.JSONDecodeError):
                # Do not let malformed legacy rows break the public leaderboard.
                continue
            agent_id = str(result.get("agent_id", "unknown"))
            groups.setdefault(agent_id, []).append(result)

        board: list[dict[str, Any]] = []
        for agent_id, results in groups.items():
            durations = [float(r.get("metrics", {}).get("duration_ms", 0)) for r in results]
            costs = [float(r.get("metrics", {}).get("cost_usd", 0)) for r in results]
            scores = [float(r.get("score", 0)) for r in results]
            successes = sum(1 for r in results if r.get("success"))
            board.append(
                {
                    "agent_id": agent_id,
                    "runs": len(results),
                    "success_rate": successes / len(results),
                    "mean_score": sum(scores) / len(scores),
                    "median_duration_ms": median(durations),
                    "total_cost_usd": sum(costs),
                }
            )
        return sorted(
            board,
            key=lambda item: (
                -float(item["mean_score"]),
                -float(item["success_rate"]),
                float(item["total_cost_usd"]),
                float(item["median_duration_ms"]),
            ),
        )
