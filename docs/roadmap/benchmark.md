# BEN — Benchmark, evaluation and offline competition

**Scope:** `benchmark/`, `suite.py`, `scoring.py`, `benchmarks/`, `docs/tsec.md`. A generic adapter and TSec adapter, pass@k suites, and Docker verifiers already exist.

### BEN-01 [ ] P1 — Common benchmark lifecycle
Deliver: stable discover/provision/submit/evaluate/teardown contracts with result ownership and idempotent cleanup; no SDK code in domain/runtime.
Accept: contract tests for unavailable service, failed provision, submission retry, timeout, teardown after crash and unexpected API response.

### BEN-02 [ ] P1 — Offline readiness gate
Deliver: preflight checks for images, tools, model access, task inputs and SDK integration without mandatory public Internet; clear run-time egress policy and diagnostics.
Accept: exercise authorized target-only environment with public egress disabled; produce actionable missing-dependency errors before a run.

### BEN-03 [ ] P1 — Cross-domain evaluation and baseline
Deliver: fixtures across Web, Pwn, Reverse, Crypto, Forensics, Misc plus non-security coding/file tasks where feasible; frozen seeds/time/tool/token budgets and fixed scoring contracts.
Accept: per-domain success/pass@k, retries, false-positive verification, time, tokens and cost; CI smoke tests and periodic full-suite report without overfitting one benchmark.

### BEN-04 [ ] P2 — Reproducibility
Deliver: immutable task/model/container/tool manifests, grader versions and output artifact hashes; distinguish environmental failures from solver failures.
Accept: repeated runs produce comparable metadata and auditable variance; secrets scrubbed.

**Non-goals:** hiding task-specific heuristics in universal solver, evaluation using unverified Agent self-reports.
