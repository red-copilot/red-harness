# QA — Testing, architecture and performance

**Scope:** `tests/`, `.github/workflows/ci.yml`, `pyproject.toml`, `docs/architecture.md`. Existing architecture import guard and Pi/Docker/SDK smoke workflows should remain.

### QA-01 [ ] P0 — Enforced correctness gates
Deliver: tests for trust separation, lifecycle transitions, world event replay, checkpoint crashes, verification false positives, offline restrictions and module dependency direction.
Accept: CI rejects regressions; record exact local commands/results in each PR. Distinguish skipped Docker/SDK tests from passing tests.

### QA-02 [ ] P1 — Evaluation harness for harness changes
Deliver: frozen fixture manifest, seeds, budgets, software/model/container versions, metrics summaries and baseline comparison scripts.
Accept: A/B repeats show result distribution by domain, and any change degrading safety or verification correctness blocks merge.

### QA-03 [ ] P2 — Performance and chaos
Deliver: synthetic concurrent readers/writers, large event streams, worker kills and disk-full or truncated-artifact injection.
Accept: bounded memory/latency targets documented from measured baseline, clear fail-closed recovery, no unhandled corruption.

### QA-04 [ ] continuous — Architectural hygiene
Deliver: enforce import direction; no redundant wrappers, duplicate lifecycle logic or premature infrastructure. Document public contract migrations.
Accept: dependency checks in CI; PRs that add modules justify ownership, public API and tests.

## Suggested validation sequence
`python -m pip install -e ".[dev]"`; run relevant `pytest` subset, `pytest tests/test_architecture.py`, `ruff check .`, then `pytest`. Inspect actual CI workflow before assuming exact supported commands. Docker/TSec tests run only in prepared environments and must be explicitly reported as unexecuted otherwise.
