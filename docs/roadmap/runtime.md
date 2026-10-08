# RUN — Runtime, solver and recovery

**Scope:** `solver_loop.py`, `runtime/`, `progress.py`, `session.py`, `orchestrator.py`, `benchmark/runner.py`. Shared bootstrap, lifecycle policies, checkpoints and processed-event ID tracking already exist.

### RUN-01 [ ] P0 — Explicit run state machine
Deliver: define allowed transitions among created/running/paused/recovering/verifying/succeeded/failed/exhausted/cancelled; separate pure decision from event normalization and disk/Agent side effects. Use one lifecycle policy for local and benchmark paths.
Accept: table-driven transition tests, illegal transition rejection, exactly one terminal decision, cancellation vs success race tests; no benchmark import in runtime.

### RUN-02 [ ] P0 — Crash-safe checkpoints and action reconciliation
Deliver: durable agent cursor, World revision, budget, feedback cursor and in-flight action IDs; recovery protocol for action dispatch/effect/feedback/checkpoint crash windows. Already available file checkpoints and explicit operator reconciliation are a base, **not** exactly-once crash resume.
Accept: fault-injection tests across every boundary; unknown effect never automatically retried; mismatch and corrupt checkpoint detected; no double billing/events on duplicate ingestion.

### RUN-03 [ ] P1 — Event processing and bounded autonomy
Deliver: dedupe with persisted identity, stable ordering and world-event revisions; explicit continue/verify/replan/stop decisions; bounded replan frequency and budget/time thresholds.
Accept: duplicate/reordered event tests, no progress loops terminate appropriately, goal verification dominates generic completion, reproducible trace transitions.

### RUN-04 [ ] P2 — Resource ownership
Deliver: one owner for container/session/gateway/task teardown, consistent timeouts and cancellation, terminal persistence idempotency.
Accept: forced kill, signal interruption and external benchmark teardown tests; no leaked resources or overwritten terminal results.

**Non-goals:** distributed workflow orchestrator, required DAG, implementation of Pi's internal reasoning loop.
