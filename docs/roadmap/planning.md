# PLN — Planning, skills and coordination

**Scope:** `planner.py`, `skills.py`, `progress.py`, `coordination.py`, `solver_loop.py`. Rolling horizon 1–3 and heuristic skill ranking already exist.

### PLN-01 [ ] P1 — Replan on actual information change
Deliver: decisions reference goal, world revision, verified/claimed evidence, failures and remaining budget. Distinguish real new evidence from repeated text/tool success. Rate-limit repeated plans and avoid stale skill prerequisites.
Accept: deterministic fixtures show verification failure, contradiction and capability gain yield changed decisions; no endless identical replans.

### PLN-02 [ ] P2 — Planner ablations and safe fallback
Deliver: compare Pi-only, Pi+World, Pi+World+heuristic planner on identical tasks/seeds/budgets. Keep planner optional and non-blocking when skills unavailable.
Accept: published per-domain completion/cost comparisons; if planner does not improve results, retain only optional reference implementation.

### PLN-03 [ ] P2 — Skill interfaces
Deliver: declarative preconditions/produces metadata and applicability reporting, validated schemas and explicit selection rationale; no implicit unreviewed shell execution from skill metadata.
Accept: irrelevant/stale prerequisite rejection, deterministic scoring and injection resistance.

### PLN-04 [ ] experimental — Multi-agent shared work
Use existing leased blackboard only where parallelism is demonstrably beneficial; add ownership, conflict handling and cancellation.
Accept: no duplicate externally visible actions under lease races and positive measured throughput/quality gains.

**Non-goals:** required task DAG or harness-authored chain-of-thought.
