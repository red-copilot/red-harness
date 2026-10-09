# WLD — World state and retrieval

**Scope:** `world/`, `aci.py`, `coordination.py`. SQLite event repository, reducers, inbox ingestion, provenance labels, snapshots and deterministic retriever exist. World is not assumed to be a DAG.

### WLD-01 [ ] P1 — State integrity and semantics
Deliver: preserve event authority, revision CAS and idempotence; explicit provenance tiers for claim/evidence/verified, validity windows, supersession and contradiction, plus migration tests.
Accept: deterministic replay, append/inbox duplication, concurrent-writer conflict, stale evidence and malicious trust-escalation tests.

### WLD-02 [ ] P1 — Decision-useful context
Deliver: query current goals, valid capabilities, blockers, contradictions, evidence references and relevant failures within a bounded prompt budget; deterministic output and disclosure of missing knowledge.
Accept: golden retrieval fixtures, no stale capability promoted, context stays within configured budget and correctly changes when evidence status changes.

### WLD-03 [ ] P2 — Compact storage, query and snapshots
Deliver: retention/compaction policy without losing provenance, indexed SQLite queries or FTS5 if measured useful, periodic snapshot/restore.
Accept: replay parity after compaction, large synthetic runs within stated latency/storage budgets, and corruption detection.

### WLD-04 [ ] experimental — Learning-based retrieval
Only propose after WLD-02 ablations establish retrieval bottlenecks; compare against deterministic baseline on held-out domains.
Accept: measurable success gain relative to costs, reproducible offline model availability and rollback path.

**Non-goals:** full causal simulator, mandatory graph planner, vector database without observed value.
