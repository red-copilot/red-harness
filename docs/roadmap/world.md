# WLD — World state and retrieval

**Scope:** `world/`, `aci.py`, `coordination.py`. SQLite event repository, reducers, inbox ingestion, provenance labels, snapshots and deterministic retriever exist. World is not assumed to be a DAG.

### WLD-01 [x] P1 — State integrity and semantics
Deliver: preserve event authority, revision CAS and idempotence; explicit provenance tiers for claim/evidence/verified, validity windows, supersession and contradiction, plus migration tests.
Accept: deterministic replay, append/inbox duplication, concurrent-writer conflict, stale evidence and malicious trust-escalation tests.

Acceptance is demonstrated: actor-derived provenance tiers, timezone-aware validity windows, revision CAS, idempotent event append, conflict detection and deterministic replay have regression coverage. Added explicit `contradicts` links alongside `supersedes`, rendered both links in context, and tested legacy v1 record loading. Tests cover inbox duplicate polling, SQLite duplicate IDs and concurrent stale writers, stale evidence rejection, and agent trust escalation. See [world tests](../../tests/test_world.py), [SQLite tests](../../tests/test_sqlite_world.py), and [inbox tests](../../tests/test_live_world.py). The world/retrieval focused set passed (21 tests); full suite passed (205 passed, 1 Docker integration skipped) on 2026-10-09.

### WLD-02 [x] P1 — Decision-useful context
Deliver: query current goals, valid capabilities, blockers, contradictions, evidence references and relevant failures within a bounded prompt budget; deterministic output and disclosure of missing knowledge.
Accept: golden retrieval fixtures, no stale capability promoted, context stays within configured budget and correctly changes when evidence status changes.

`WorldRetriever` and `WorldContextBuilder` provide deterministic, validity-filtered ranking and character-bounded output. Golden output, goal-relevant retrieval, expired observation and capability filtering, truncation, contradiction links, and context changes after evidence status changes are covered by [retrieval tests](../../tests/test_retrieval.py). Empty sections disclose missing records; constraints, evidence provenance, and failures are included in the bounded projection. Relevant retrieval tests passed (4 tests); full suite passed (205 passed, 1 Docker integration skipped) on 2026-10-09.

### WLD-03 [x] P2 — Compact storage, query and snapshots
Deliver: retention/compaction policy without losing provenance, indexed SQLite queries or FTS5 if measured useful, periodic snapshot/restore.
Accept: replay parity after compaction, large synthetic runs within stated latency/storage budgets, and corruption detection.

Measured the old SQLite path at over 37 seconds without completing 1,000 synthetic events: every append reloaded all materialized objects and rewrote the full event export and snapshot. SQLite now caches the materialized projection between writes, checkpoints event/snapshot exports every 500 revisions and at run finalization, supports explicit `flush`, preserves the full event/provenance history during `compact()` (WAL truncate + VACUUM), verifies SQLite/replay/export/snapshot parity, and can create a consistent SQLite backup for restore. Existing `(kind, sequence)` and primary-key indexes were retained; no FTS index was added without retrieval evidence. Each append now copies only its changed collection; the 1,000-event fixture measured 2.55 seconds with Python allocation tracing, 14,794,801 peak traced bytes, and 7,067,442 stored bytes (separate process peak RSS 51,552 KiB; untraced run: 1.48 seconds). Tests cover prior-snapshot stability, atomic concurrent reader snapshots, periodic snapshots, compaction replay parity, backup restore, and corrupt snapshot detection. Enforced bounds are under 10 seconds, 32 MiB peak traced Python allocation, and 20 MiB stored files. Docker tests were unavailable locally.

### WLD-04 [ ] experimental — Learning-based retrieval
Only propose after WLD-02 ablations establish retrieval bottlenecks; compare against deterministic baseline on held-out domains.
Accept: measurable success gain relative to costs, reproducible offline model availability and rollback path.

**Non-goals:** full causal simulator, mandatory graph planner, vector database without observed value.
