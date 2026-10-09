# OPS — Control plane, worker and observability

**Scope:** `api/`, `control_plane.py`, `queue.py`, `worker.py`, `otel.py`, `trace.py`, `telemetry.py`, `audit.py`.

### OPS-01 [ ] P2 — Durable job and worker semantics
Deliver: lease and heartbeat recovery, run ownership, cancellation, duplicate dispatch protection and idempotent terminal persistence.
Accept: kill/restart worker, lease expiration and concurrent polling tests; duplicate dispatch never silently repeats unresolved tool effects.

### OPS-02 [ ] P2 — Trace/provenance contract
Deliver: correlate run/session/action/event/evidence/plan revision; redact credentials/PII and bound output size. Keep trace export independent of internal World DB access.
Accept: JSON schema tests, redaction tests and audit of interrupted run.

### OPS-03 [ ] P2 — API safety and access boundaries
Deliver: strict authn/authz, input validation, pagination and artifact path checks; separate trusted control-plane and Agent network.
Accept: unauthorized operations rejected, traversal/symlink tests, large-input and malformed event coverage.

### OPS-04 [ ] experimental — Scale-out
Only consider broker/PostgreSQL after measured SQLite/worker limitations; stage migration behind repository/queue protocols.
Accept: evidence of bottleneck, rollback/migration test and improved throughput without weaker consistency.
