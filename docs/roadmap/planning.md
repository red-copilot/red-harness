# PLN — Planning, skills and coordination

**Scope:** `planner.py`, `skills.py`, `progress.py`, `coordination.py`, `solver_loop.py`. Rolling horizon 1–3 and heuristic skill ranking already exist.

### PLN-01 [x] P1 — Replan on actual information change
Deliver: decisions reference goal, world revision, verified/claimed evidence, failures and remaining budget. Distinguish real new evidence from repeated text/tool success. Rate-limit repeated plans and avoid stale skill prerequisites.
Accept: deterministic fixtures show verification failure, contradiction and capability gain yield changed decisions; no endless identical replans.

Implemented in `RollingHorizonPlanner` and `SolverLoop`: ranking considers current goal tokens, world records/provenance, skill failures, and remaining budget; expired or insufficiently trusted prerequisites do not satisfy a skill. Replanning follows world/evidence changes and uses a state signature to suppress identical repeats. Refuted hypotheses now set an explicit replan reason as well as annotating action triggers. Deterministic tests cover verification failure, refuted hypotheses, capability gain, failed-skill penalty, trust-qualified prerequisites, and repeated identical signals. See [planner tests](../../tests/test_planner.py) and [solver loop tests](../../tests/test_solver_loop.py). Focused planning/solver tests passed (17 tests); full suite passed (205 passed, 1 Docker integration skipped) on 2026-10-09.

### PLN-02 [~] P2 — Planner ablations and safe fallback
Deliver: compare Pi-only, Pi+World, Pi+World+heuristic planner on identical tasks/seeds/budgets. Keep planner optional and non-blocking when skills unavailable.
Accept: published per-domain completion/cost comparisons; if planner does not improve results, retain only optional reference implementation.

`SolverLoop` accepts `planner_enabled=False`; replanning requests are recorded as suppressed and the loop continues without writing or publishing a planner action. The solver composition now exposes `--solver-profile pi-only|pi-world|pi-world-heuristic` for both single runs and suites. Profile selection controls World context and heuristic planning, is stored in run and suite manifests, is checked on resume, and is reported by the evaluation comparator while preserving frozen task/seed checks. Bootstrap tests verify all three settings; comparison tests cover profile reporting and rejection of unknown profiles.

On 2026-10-09, ran the fixed-seed 18-attempt, nine-domain synthetic suite under all three profiles with identical demo-agent and budget settings. Each profile produced 18/18 verified successes, 100% score, 216 tokens, 18 tool calls, and zero false-positive verifications. Pairwise comparisons passed with no regressions. This is plumbing and regression evidence only: the deterministic demo agent does not make meaningful model decisions, so these results do not establish a planner benefit or satisfy the required model-backed per-domain completion/cost comparison. PLN-02 remains in progress pending that evidence. Focused validation: 33 bootstrap/comparison/suite/smoke tests passed. Full validation: 350 passed, 3 skipped; Ruff, compileall, architecture test (1 passed), and `git diff --check` passed. Docker-dependent tests were skipped in this environment.

The maintainer authorized the model-backed evaluation on 2026-10-09. Execution was attempted, but this workspace cannot run it: the configured Pi agent requires a Docker image, neither Docker nor the Pi CLI is installed, no supported provider key is present in the environment, and the checked-in virtualenv points to Python 3.10 although the project requires Python 3.11 (the installed Python 3.11 has no project dependencies). No model requests were made and no evaluation results are claimed. Repeat the fixed-seed three-profile suite once a Python 3.11 environment with dependencies, the Pi image/runtime, and provider credentials is available.

### PLN-03 [x] P2 — Skill interfaces
Deliver: declarative preconditions/produces metadata and applicability reporting, validated schemas and explicit selection rationale; no implicit unreviewed shell execution from skill metadata.
Accept: irrelevant/stale prerequisite rejection, deterministic scoring and injection resistance.

Skill and selector schemas reject unknown top-level execution fields; the planner consumes skill metadata only and never executes a command from it. `HeuristicSkillPlanner.explain` reports applicability and reason codes for missing, stale, scope-mismatched and insufficient-trust prerequisites, and the plan API exposes that report beside ranked candidates. Ranked actions include deterministic selection rationale. Tests cover stale/untrusted/missing preconditions, rejection of an unreviewed top-level `command`, inert nested command metadata, deterministic repeated scores, and candidate rationale. Acceptance criteria are met; see [skill tests](../../tests/test_skills.py), [planner tests](../../tests/test_planner.py), and [control-plane tests](../../tests/test_control_plane.py). Validation on 2026-10-09: focused tests passed (26); full suite passed (352 passed, 3 skipped); Ruff, compileall, architecture test (1 passed), and `git diff --check` passed. Docker-dependent tests were skipped locally.

### PLN-04 [ ] experimental — Multi-agent shared work
Use existing leased blackboard only where parallelism is demonstrably beneficial; add ownership, conflict handling and cancellation.
Accept: no duplicate externally visible actions under lease races and positive measured throughput/quality gains.

**Non-goals:** required task DAG or harness-authored chain-of-thought.
