from __future__ import annotations

import json
import sqlite3
import threading
import time
from pathlib import Path

from pydantic import BaseModel

from .models import WorldEvent, WorldObjectKind, WorldSnapshot
from .reducer import WorldReducer
from .repository import WorldConflictError


class SQLiteWorldRepository:
    """Transactional event-sourced WorldRepository backed by SQLite/WAL."""

    def __init__(self, path: Path, *, snapshot_interval: int = 500) -> None:
        if snapshot_interval < 1:
            raise ValueError("snapshot_interval must be positive")
        self.snapshot_interval = snapshot_interval
        self.db_path = (
            path if path.suffix == ".db" else path.with_name("world.db")
        )
        self.export_event_path = (
            path if path.suffix != ".db" else path.with_name("world.events.jsonl")
        )
        self.snapshot_path = self.db_path.with_name("world.snapshot.json")
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._reducer = WorldReducer()
        self._initialize()
        self._import_legacy_jsonl_if_needed()
        self._snapshot_cache = self.snapshot

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.db_path,
            timeout=5.0,
            isolation_level=None,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA busy_timeout=5000")
        connection.execute("PRAGMA synchronous=NORMAL")
        return connection

    def _initialize(self) -> None:
        deadline = time.monotonic() + 5.0
        while True:
            try:
                with self._connect() as connection:
                    connection.execute("PRAGMA journal_mode=WAL")
                    connection.executescript(
                        """
                        CREATE TABLE IF NOT EXISTS world_events (
                            sequence INTEGER PRIMARY KEY AUTOINCREMENT,
                            event_id TEXT NOT NULL UNIQUE,
                            schema_version TEXT NOT NULL,
                            ts TEXT NOT NULL,
                            kind TEXT NOT NULL,
                            op TEXT NOT NULL,
                            object_id TEXT NOT NULL,
                            payload_json TEXT NOT NULL,
                            actor TEXT NOT NULL,
                            source_event_id TEXT
                        );

                        CREATE INDEX IF NOT EXISTS idx_world_events_kind
                            ON world_events(kind, sequence);

                        CREATE TABLE IF NOT EXISTS world_objects (
                            kind TEXT NOT NULL,
                            object_id TEXT NOT NULL,
                            revision INTEGER NOT NULL,
                            payload_json TEXT NOT NULL,
                            PRIMARY KEY (kind, object_id)
                        );

                        CREATE TABLE IF NOT EXISTS world_meta (
                            key TEXT PRIMARY KEY,
                            value TEXT NOT NULL
                        );

                        INSERT OR IGNORE INTO world_meta(key, value)
                            VALUES ('revision', '0');
                        """
                    )
                return
            except sqlite3.OperationalError as exc:
                if "locked" not in str(exc).lower() or time.monotonic() >= deadline:
                    raise
                time.sleep(0.05)

    @property
    def snapshot(self) -> WorldSnapshot:
        with self._connect() as connection:
            # Revision and materialized rows must come from the same SQLite
            # read snapshot while writers commit concurrently in WAL mode.
            connection.execute("BEGIN")
            snapshot = self._load_snapshot(connection)
            self._snapshot_cache = snapshot
            return snapshot.model_copy(deep=True)

    @property
    def revision(self) -> int:
        with self._connect() as connection:
            return self._current_revision(connection)

    def _current_revision(self, connection: sqlite3.Connection) -> int:
        row = connection.execute(
            "SELECT value FROM world_meta WHERE key='revision'"
        ).fetchone()
        return int(row["value"]) if row is not None else 0

    def _load_snapshot(self, connection: sqlite3.Connection) -> WorldSnapshot:
        snapshot = WorldSnapshot(revision=self._current_revision(connection))
        collection_names = {
            "entity": "entities",
            "relation": "relations",
            "observation": "observations",
            "artifact": "artifacts",
            "capability": "capabilities",
            "hypothesis": "hypotheses",
            "goal": "goals",
            "action": "actions",
            "constraint": "constraints",
            "failure": "failures",
        }
        rows = connection.execute(
            "SELECT kind, object_id, payload_json FROM world_objects "
            "ORDER BY kind, object_id"
        ).fetchall()
        for row in rows:
            kind = row["kind"]
            collection = getattr(snapshot, collection_names[kind])
            payload = json.loads(row["payload_json"])
            # Reuse the reducer's validation/provenance model without changing revision.
            temp = WorldSnapshot()
            event = WorldEvent(
                id=payload.get("provenance", {}).get("event_id") or f"load:{row['object_id']}",
                kind=kind,
                op="upsert",
                object=payload,
                actor=payload.get("provenance", {}).get("actor") or "harness",
                source_event_id=payload.get("provenance", {}).get("source_event_id"),
            )
            self._reducer.apply(temp, event)
            source_collection = getattr(temp, collection_names[kind])
            collection[row["object_id"]] = source_collection[row["object_id"]]
        return snapshot

    @staticmethod
    def _snapshot_view(snapshot: WorldSnapshot) -> WorldSnapshot:
        """Return a detached collection view without re-copying every record."""
        view = snapshot.model_copy(deep=False)
        for name in (
            "entities",
            "relations",
            "observations",
            "artifacts",
            "capabilities",
            "hypotheses",
            "goals",
            "actions",
            "constraints",
            "failures",
        ):
            setattr(view, name, dict(getattr(snapshot, name)))
        return view

    def events(
        self,
        *,
        limit: int | None = None,
        after_sequence: int | None = None,
    ) -> list[WorldEvent]:
        query = "SELECT payload_json FROM world_events"
        params: list[object] = []
        if after_sequence is not None:
            query += " WHERE sequence > ?"
            params.append(after_sequence)
        query += " ORDER BY sequence"
        if limit is not None:
            query += " LIMIT ?"
            params.append(limit)
        with self._connect() as connection:
            rows = connection.execute(query, params).fetchall()
        return [WorldEvent.model_validate_json(row["payload_json"]) for row in rows]

    def sequenced_events(
        self,
        *,
        limit: int | None = None,
        after_sequence: int | None = None,
    ) -> list[tuple[int, WorldEvent]]:
        query = "SELECT sequence, payload_json FROM world_events"
        params: list[object] = []
        if after_sequence is not None:
            query += " WHERE sequence > ?"
            params.append(after_sequence)
        query += " ORDER BY sequence"
        if limit is not None:
            query += " LIMIT ?"
            params.append(limit)
        with self._connect() as connection:
            rows = connection.execute(query, params).fetchall()
        return [
            (int(row["sequence"]), WorldEvent.model_validate_json(row["payload_json"]))
            for row in rows
        ]

    def replay(self) -> WorldSnapshot:
        return self._reducer.replay(self.events())

    def append(
        self,
        event: WorldEvent,
        *,
        expected_revision: int | None = None,
    ) -> WorldSnapshot:
        object_id = event.object.get("id")
        if not isinstance(object_id, str) or not object_id:
            raise ValueError("world events require object.id")

        payload_json = event.model_dump_json()
        with self._lock, self._connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            try:
                current_revision = self._current_revision(connection)
                existing = connection.execute(
                    "SELECT payload_json FROM world_events WHERE event_id=?",
                    (event.id,),
                ).fetchone()
                if existing is not None:
                    if existing["payload_json"] != payload_json:
                        raise ValueError(
                            f"world event id {event.id} already exists with different payload"
                        )
                    connection.execute("COMMIT")
                    snapshot = self._load_snapshot(connection)
                    self._snapshot_cache = snapshot
                    return self._snapshot_view(snapshot)

                if (
                    expected_revision is not None
                    and expected_revision != current_revision
                ):
                    raise WorldConflictError(
                        f"world revision conflict: expected {expected_revision}, "
                        f"current {current_revision}"
                    )

                cached = getattr(self, "_snapshot_cache", None)
                if cached is not None and cached.revision == current_revision:
                    snapshot = cached.model_copy(deep=False)
                    collection_names = {
                        "entity": "entities",
                        "relation": "relations",
                        "observation": "observations",
                        "artifact": "artifacts",
                        "capability": "capabilities",
                        "hypothesis": "hypotheses",
                        "goal": "goals",
                        "action": "actions",
                        "constraint": "constraints",
                        "failure": "failures",
                    }
                    collection_name = collection_names[event.kind]
                    setattr(snapshot, collection_name, dict(getattr(cached, collection_name)))
                else:
                    snapshot = self._load_snapshot(connection)
                # The changed collection was detached above. The reducer only
                # replaces or removes one record in that collection and updates
                # the revision, so copying every record here adds quadratic work
                # to long append streams without improving snapshot isolation.
                candidate = snapshot
                self._reducer.apply(candidate, event)
                new_revision = candidate.revision

                connection.execute(
                    """
                    INSERT INTO world_events(
                        event_id, schema_version, ts, kind, op, object_id,
                        payload_json, actor, source_event_id
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        event.id,
                        event.schema_version,
                        event.ts.isoformat(),
                        event.kind,
                        event.op,
                        object_id,
                        payload_json,
                        event.actor,
                        event.source_event_id,
                    ),
                )

                if event.op == "remove":
                    connection.execute(
                        "DELETE FROM world_objects WHERE kind=? AND object_id=?",
                        (event.kind, object_id),
                    )
                else:
                    collection_names = {
                        "entity": "entities",
                        "relation": "relations",
                        "observation": "observations",
                        "artifact": "artifacts",
                        "capability": "capabilities",
                        "hypothesis": "hypotheses",
                        "goal": "goals",
                        "action": "actions",
                        "constraint": "constraints",
                        "failure": "failures",
                    }
                    record = getattr(candidate, collection_names[event.kind])[object_id]
                    record_json = json.dumps(
                        record.model_dump(mode="json"),
                        ensure_ascii=False,
                        separators=(",", ":"),
                    )
                    connection.execute(
                        """
                        INSERT INTO world_objects(kind, object_id, revision, payload_json)
                        VALUES (?, ?, ?, ?)
                        ON CONFLICT(kind, object_id) DO UPDATE SET
                            revision=excluded.revision,
                            payload_json=excluded.payload_json
                        """,
                        (event.kind, object_id, new_revision, record_json),
                    )

                connection.execute(
                    "UPDATE world_meta SET value=? WHERE key='revision'",
                    (str(new_revision),),
                )
                connection.execute("COMMIT")
                self._snapshot_cache = candidate
            except Exception:
                connection.execute("ROLLBACK")
                raise

        if new_revision % self.snapshot_interval == 0:
            self.flush()
        return self._snapshot_view(candidate)

    def upsert(
        self,
        kind: WorldObjectKind,
        obj: BaseModel | dict,
        *,
        actor: str = "harness",
        source_event_id: str | None = None,
        expected_revision: int | None = None,
    ) -> WorldSnapshot:
        data = obj.model_dump(mode="json") if isinstance(obj, BaseModel) else dict(obj)
        return self.append(
            WorldEvent(
                kind=kind,
                op="upsert",
                object=data,
                actor=actor,
                source_event_id=source_event_id,
            ),
            expected_revision=expected_revision,
        )

    def remove(
        self,
        kind: WorldObjectKind,
        object_id: str,
        *,
        actor: str = "harness",
        expected_revision: int | None = None,
    ) -> WorldSnapshot:
        return self.append(
            WorldEvent(kind=kind, op="remove", object={"id": object_id}, actor=actor),
            expected_revision=expected_revision,
        )

    def _import_legacy_jsonl_if_needed(self) -> None:
        if self.revision != 0 or not self.export_event_path.is_file():
            return
        lines = [
            line
            for line in self.export_event_path.read_text(encoding="utf-8").splitlines()
            if line.strip()
        ]
        for line in lines:
            self.append(WorldEvent.model_validate_json(line))

    def _write_exports(self) -> None:
        with self._connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            try:
                rows = connection.execute(
                    "SELECT payload_json FROM world_events ORDER BY sequence"
                ).fetchall()
                events = [
                    WorldEvent.model_validate_json(row["payload_json"])
                    for row in rows
                ]
                snapshot = self._load_snapshot(connection)

                temp_events = self.export_event_path.with_suffix(
                    self.export_event_path.suffix + ".tmp"
                )
                temp_events.write_text(
                    "".join(event.model_dump_json() + "\n" for event in events),
                    encoding="utf-8",
                )
                temp_events.replace(self.export_event_path)

                temp_snapshot = self.snapshot_path.with_suffix(
                    self.snapshot_path.suffix + ".tmp"
                )
                temp_snapshot.write_text(
                    json.dumps(
                        snapshot.model_dump(mode="json"),
                        ensure_ascii=False,
                        indent=2,
                    )
                    + "\n",
                    encoding="utf-8",
                )
                temp_snapshot.replace(self.snapshot_path)
                connection.execute("COMMIT")
            except Exception:
                connection.execute("ROLLBACK")
                raise

    def flush(self) -> None:
        """Write current event and snapshot exports at a checkpoint boundary."""
        self._write_exports()

    def integrity_check(self) -> dict[str, object]:
        """Check SQLite structure and parity among events, objects, and exports."""
        with self._connect() as connection:
            sqlite_results = [
                str(row[0]) for row in connection.execute("PRAGMA integrity_check").fetchall()
            ]
            materialized = self._load_snapshot(connection)
        replayed = self.replay()
        issues: list[str] = []
        if sqlite_results != ["ok"]:
            issues.append("sqlite_integrity_check_failed")
        if replayed != materialized:
            issues.append("event_replay_mismatch")
        if self.snapshot_path.exists():
            try:
                exported_snapshot = WorldSnapshot.model_validate_json(
                    self.snapshot_path.read_text(encoding="utf-8")
                )
            except (OSError, ValueError):
                issues.append("snapshot_corrupt")
            else:
                if exported_snapshot != materialized:
                    issues.append("snapshot_mismatch")
        if self.export_event_path.exists():
            try:
                exported_events = [
                    WorldEvent.model_validate_json(line)
                    for line in self.export_event_path.read_text(encoding="utf-8").splitlines()
                    if line.strip()
                ]
            except (OSError, ValueError):
                issues.append("event_export_corrupt")
            else:
                if self._reducer.replay(exported_events) != materialized:
                    issues.append("event_export_mismatch")
        return {
            "ok": not issues,
            "sqlite": sqlite_results,
            "revision": materialized.revision,
            "event_count": len(self.events()),
            "issues": issues,
        }

    def backup_to(self, destination: Path) -> Path:
        """Create a transactionally consistent SQLite backup for restore/testing."""
        destination = Path(destination)
        destination.parent.mkdir(parents=True, exist_ok=True)
        self.flush()
        with self._connect() as source, sqlite3.connect(destination) as target:
            source.backup(target)
        restored = SQLiteWorldRepository(destination, snapshot_interval=self.snapshot_interval)
        restored.flush()
        report = restored.integrity_check()
        if not report["ok"]:
            raise ValueError(f"World backup failed integrity verification: {report['issues']}")
        return destination

    def compact(self) -> dict[str, object]:
        """Reclaim SQLite pages without deleting event or provenance history."""
        self.flush()
        with self._connect() as connection:
            connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            connection.execute("VACUUM")
        report = self.integrity_check()
        if not report["ok"]:
            raise ValueError(f"World compaction failed integrity verification: {report['issues']}")
        return report
