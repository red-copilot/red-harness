from __future__ import annotations

import json
import sqlite3
import time
import uuid
from pathlib import Path
from statistics import median
from typing import Any

from pydantic import BaseModel, Field


class GatewayJobSpec(BaseModel):
    enabled: bool = False
    policy_path: str | None = None
    mode: str = "host"
    sidecar_image: str | None = None
    sidecar_runtime: str | None = None
    model_upstream: str | None = None
    model_api_key_env: str = "REDHARNESS_MODEL_API_KEY"
    input_price_per_million_usd: float = Field(default=0.0, ge=0)
    output_price_per_million_usd: float = Field(default=0.0, ge=0)


class JobPayload(BaseModel):
    task_path: str
    agent_path: str
    runs_root: str = ".redharness/runs"
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
        connection.execute("PRAGMA journal_mode=WAL")
        connection.execute("PRAGMA busy_timeout=30000")
        return connection

    def _init_db(self) -> None:
        with self._connect() as db:
            db.execute(
                """
                CREATE TABLE IF NOT EXISTS jobs (
                    id TEXT PRIMARY KEY,
                    state TEXT NOT NULL,
                    payload_json TEXT NOT NULL,
                    result_json TEXT,
                    error TEXT,
                    lease_owner TEXT,
                    lease_expires_at REAL,
                    attempts INTEGER NOT NULL DEFAULT 0,
                    created_at REAL NOT NULL,
                    updated_at REAL NOT NULL
                )
                """
            )
            db.execute(
                "CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(state, lease_expires_at, created_at)"
            )

    @staticmethod
    def _row(row: sqlite3.Row | None) -> dict[str, Any] | None:
        if row is None:
            return None
        return {
            "id": row["id"],
            "state": row["state"],
            "payload": json.loads(row["payload_json"]),
            "result": json.loads(row["result_json"]) if row["result_json"] else None,
            "error": row["error"],
            "lease_owner": row["lease_owner"],
            "lease_expires_at": row["lease_expires_at"],
            "attempts": row["attempts"],
            "created_at": row["created_at"],
            "updated_at": row["updated_at"],
        }

    def submit(self, payload: JobPayload) -> dict[str, Any]:
        now = time.time()
        job_id = f"job_{uuid.uuid4().hex}"
        with self._connect() as db:
            db.execute(
                """
                INSERT INTO jobs(id, state, payload_json, created_at, updated_at)
                VALUES (?, 'queued', ?, ?, ?)
                """,
                (job_id, payload.model_dump_json(), now, now),
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
        now = time.time()
        expires = now + lease_seconds
        with self._connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute(
                """
                SELECT * FROM jobs
                WHERE state = 'queued'
                   OR (state = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?)
                ORDER BY created_at
                LIMIT 1
                """,
                (now,),
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

    def heartbeat(self, job_id: str, worker_id: str, *, lease_seconds: int = 60) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET lease_expires_at=?, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (now + lease_seconds, now, job_id, worker_id),
            )
        return cursor.rowcount == 1

    def complete(self, job_id: str, worker_id: str, result: dict[str, Any]) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET state='completed', result_json=?, error=NULL,
                    lease_owner=NULL, lease_expires_at=NULL, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (json.dumps(result, ensure_ascii=False), now, job_id, worker_id),
            )
        return cursor.rowcount == 1

    def fail(self, job_id: str, worker_id: str, error: str) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE jobs
                SET state='failed', error=?, lease_owner=NULL,
                    lease_expires_at=NULL, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (error[:4000], now, job_id, worker_id),
            )
        return cursor.rowcount == 1

    def leaderboard(self) -> list[dict[str, Any]]:
        with self._connect() as db:
            rows = db.execute(
                "SELECT result_json FROM jobs WHERE state='completed' AND result_json IS NOT NULL"
            ).fetchall()

        groups: dict[str, list[dict[str, Any]]] = {}
        for row in rows:
            result = json.loads(row["result_json"])
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
