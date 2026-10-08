# Harness subsystem roadmaps

Baseline: main at `96cf0c056e2de30de8436fd03caefcf6d9b16d2f` (2026-10-08). Review current code before starting; implementation may have moved. Architecture target: an **evidence-driven, resumable Python modular monolith**. Pi owns agent reasoning; Harness owns lifecycle, execution policy, world records, verification, budgets, traces and recovery. No special treatment for TSec benchmark.

## Source of truth and use
- This index defines priority, dependencies, success metrics and sequencing; each subsystem file defines implementation slices.
- [Codex contract](../../AGENTS.md) prescribes selection, test, PR, stop and update rules.
- Existing architecture: [docs/architecture.md](../architecture.md), [docs/world-state.md](../world-state.md), [docs/pi.md](../pi.md), [docs/tsec.md](../tsec.md).
- Roadmap statuses are proposals based on code inspection, not claims of missing behavior. Check existing tests and functionality first.

## Workstream index
| ID | Roadmap | Priority | Dependencies |
|---|---|---|---|
| VER | [Verification & evidence](verification.md) | P0 | none |
| RUN | [Runtime & recovery](runtime.md) | P0 | VER contracts |
| EXE | [Agent & execution isolation](execution.md) | P0 | RUN events |
| WLD | [World state & retrieval](world.md) | P1 | VER provenance |
| PLN | [Planning & skills](planning.md) | P1 | WLD |
| BEN | [Benchmark, evaluation & offline](benchmark.md) | P1 | VER, EXE |
| OPS | [Control plane, worker & observability](operations.md) | P2 | RUN |
| QA | [Quality, architectural boundaries & performance](quality.md) | continuous | all |

## Dependency-aware delivery stages
- **S0 — correctness:** VER-01/02, RUN-01/02, EXE-01, QA-01. Objective success must rely on independent evidence; recoverable durable control state.
- **S1 — bounded autonomy:** RUN-03, WLD-01/02, PLN-01, BEN-01/02. State changes must actually affect next actions; no unsafe retry or network assumptions.
- **S2 — measurable generality:** BEN-03, PLN-02, EXE-02, OPS-01, QA-02/03. Demonstrate improvements across domains, not one benchmark.
- **S3 — optional experiments:** learned retrieval, planning, distributed agents or alternate stores only if S2 ablations show a measured gap.

## Global acceptance gates
1. Verification: no Agent-authored strings/tool exit status alone can mark an action effect or goal verified; each verdict references independent recorded evidence.
2. Recovery: crash points before/after action dispatch, effect, feedback and checkpoint do not silently replay ambiguous side effects.
3. Isolation: Agent cannot write canonical database, scoring results or secrets; egress policy is explicit.
4. Replay/audit: deterministic reduction of authoritative events; conflicting IDs, missing provenance and corrupt checkpoints are surfaced.
5. Evaluation: measure per-domain success/pass@k, false-verification rate, recovery correctness, wall clock, tokens, tool calls and cost on fixed seeds; publish the baseline and regression threshold.
6. Architecture: domain modules never import API, CLI, TSec SDK or Docker implementations; adapter contracts are tested.

## Routine for Codex
Identify an unblocked item -> inspect the current tree -> create focused tests -> implement -> run tests -> document evidence -> open PR -> update status **only after acceptance**. Do not start a second dependent item if acceptance has not been established. Do not directly merge to main; request review of changes to trust boundaries or public contracts.

## Definition of done
A task is done when its observable behavior, automated tests (positive, negative and recovery where relevant), docs, and CI evidence meet the item's criteria. A roadmap checkbox or green lint alone is not completion.
