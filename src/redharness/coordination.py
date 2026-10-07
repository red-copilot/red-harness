from __future__ import annotations

import json
import sqlite3
import time
import uuid
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, Field


class WorkItemSpec(BaseModel):
    run_id: str = Field(min_length=1)
    goal_id: str | None = None
    skill_id: str | None = None
    description: str = Field(min_length=1)
    priority: float = Field(default=0.5, ge=0.0, le=1.0)
    metadata: dict[str, Any] = Field(default_factory=dict)


class CoordinationStore:
    """SQLite-backed blackboard coordination queue with transactional leases."""

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
                CREATE TABLE IF NOT EXISTS work_items (
                    id TEXT PRIMARY KEY,
                    run_id TEXT NOT NULL,
                    goal_id TEXT,
                    skill_id TEXT,
                    description TEXT NOT NULL,
                    priority REAL NOT NULL,
                    metadata_json TEXT NOT NULL,
                    state TEXT NOT NULL,
                    lease_owner TEXT,
                    lease_expires_at REAL,
                    attempts INTEGER NOT NULL DEFAULT 0,
                    result_json TEXT,
                    error TEXT,
                    created_at REAL NOT NULL,
                    updated_at REAL NOT NULL
                )
                """
            )
            db.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_work_items_claim
                ON work_items(run_id, state, priority DESC, lease_expires_at, created_at)
                """
            )

    @staticmethod
    def _row(row: sqlite3.Row | None) -> dict[str, Any] | None:
        if row is None:
            return None
        return {
            "id": row["id"],
            "run_id": row["run_id"],
            "goal_id": row["goal_id"],
            "skill_id": row["skill_id"],
            "description": row["description"],
            "priority": row["priority"],
            "metadata": json.loads(row["metadata_json"]),
            "state": row["state"],
            "lease_owner": row["lease_owner"],
            "lease_expires_at": row["lease_expires_at"],
            "attempts": row["attempts"],
            "result": json.loads(row["result_json"]) if row["result_json"] else None,
            "error": row["error"],
            "created_at": row["created_at"],
            "updated_at": row["updated_at"],
        }

    def submit(self, spec: WorkItemSpec) -> dict[str, Any]:
        now = time.time()
        work_id = f"work_{uuid.uuid4().hex}"
        with self._connect() as db:
            db.execute(
                """
                INSERT INTO work_items(
                    id, run_id, goal_id, skill_id, description, priority,
                    metadata_json, state, created_at, updated_at
                )
                VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)
                """,
                (
                    work_id,
                    spec.run_id,
                    spec.goal_id,
                    spec.skill_id,
                    spec.description,
                    spec.priority,
                    json.dumps(spec.metadata, ensure_ascii=False),
                    now,
                    now,
                ),
            )
        return self.get(work_id)  # type: ignore[return-value]

    def get(self, work_id: str) -> dict[str, Any] | None:
        with self._connect() as db:
            row = db.execute(
                "SELECT * FROM work_items WHERE id = ?",
                (work_id,),
            ).fetchone()
        return self._row(row)

    def list(
        self,
        *,
        run_id: str | None = None,
        state: Literal["queued", "running", "completed", "failed"] | None = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        query = "SELECT * FROM work_items"
        where: list[str] = []
        params: list[Any] = []
        if run_id is not None:
            where.append("run_id = ?")
            params.append(run_id)
        if state is not None:
            where.append("state = ?")
            params.append(state)
        if where:
            query += " WHERE " + " AND ".join(where)
        query += " ORDER BY priority DESC, created_at LIMIT ?"
        params.append(limit)
        with self._connect() as db:
            rows = db.execute(query, tuple(params)).fetchall()
        return [self._row(row) for row in rows if row is not None]  # type: ignore[misc]

    def claim(
        self,
        run_id: str,
        agent_id: str,
        *,
        lease_seconds: int = 60,
    ) -> dict[str, Any] | None:
        now = time.time()
        expires = now + lease_seconds
        with self._connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute(
                """
                SELECT * FROM work_items
                WHERE run_id = ?
                  AND (
                    state = 'queued'
                    OR (
                      state = 'running'
                      AND lease_expires_at IS NOT NULL
                      AND lease_expires_at < ?
                    )
                  )
                ORDER BY priority DESC, created_at
                LIMIT 1
                """,
                (run_id, now),
            ).fetchone()
            if row is None:
                db.execute("COMMIT")
                return None
            db.execute(
                """
                UPDATE work_items
                SET state='running', lease_owner=?, lease_expires_at=?,
                    attempts=attempts+1, updated_at=?, error=NULL
                WHERE id=?
                """,
                (agent_id, expires, now, row["id"]),
            )
            db.execute("COMMIT")
        return self.get(str(row["id"]))

    def heartbeat(
        self,
        work_id: str,
        agent_id: str,
        *,
        lease_seconds: int = 60,
    ) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE work_items
                SET lease_expires_at=?, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (now + lease_seconds, now, work_id, agent_id),
            )
        return cursor.rowcount == 1

    def complete(self, work_id: str, agent_id: str, result: dict[str, Any]) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE work_items
                SET state='completed', result_json=?, error=NULL,
                    lease_owner=NULL, lease_expires_at=NULL, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (
                    json.dumps(result, ensure_ascii=False),
                    now,
                    work_id,
                    agent_id,
                ),
            )
        return cursor.rowcount == 1

    def fail(self, work_id: str, agent_id: str, error: str, *, requeue: bool = False) -> bool:
        now = time.time()
        state = "queued" if requeue else "failed"
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE work_items
                SET state=?, error=?, lease_owner=NULL,
                    lease_expires_at=NULL, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (state, error[:4000], now, work_id, agent_id),
            )
        return cursor.rowcount == 1

    def release(self, work_id: str, agent_id: str) -> bool:
        now = time.time()
        with self._connect() as db:
            cursor = db.execute(
                """
                UPDATE work_items
                SET state='queued', lease_owner=NULL,
                    lease_expires_at=NULL, updated_at=?
                WHERE id=? AND state='running' AND lease_owner=?
                """,
                (now, work_id, agent_id),
            )
        return cursor.rowcount == 1
