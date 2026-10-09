# Codex operating contract

Start with [docs/roadmap/README.md](docs/roadmap/README.md), then read the subsystem roadmap relevant to the requested task, docs/architecture.md, and existing tests. This document is guidance, not permission to run autonomously outside a requested task or to merge without review.

## Iteration algorithm
1. Inspect main and existing behavior; select the highest-priority **unblocked** roadmap item. Do not assume an item is incomplete without checking code/tests.
2. Open a focused branch/PR. Prefer one roadmap item per PR; avoid unrelated refactoring.
3. Write a failing regression or acceptance test first when feasible, then implement the smallest fix.
4. Execute targeted tests, architecture tests, lint, and the full feasible suite. Note commands, failures, and environment-dependent exclusions in PR.
5. Update the item status/evidence in its roadmap, with links to tests/PRs and measured before/after impact. Mark done only on demonstrated acceptance criteria.
6. Review invariants: untrusted Agent output cannot become verified evidence; retries cannot duplicate unsafe effects; benchmarks cannot leak into core; agent containers cannot mutate authoritative state.
7. Halt and request maintainer review if the task changes public contracts, destructive data migrations, credentials, network permissions, verification trust boundaries, or evaluation semantics.

## Engineering rules
- Python modular monolith. No premature microservices, mandatory DAG, neural world model, or vector database.
- Keep the Pi agent's reasoning loop separate from Harness run control. Domain state in World; ephemeral control state in Runtime; objective verdicts in Verification.
- Preserve backward-compatible import paths and event schemas until a versioned migration and tests exist.
- Keep TSec integration under benchmark adapters. Test offline operation; no install-at-runtime assumptions.
- Claims, tool exits, action-effect verification and goal completion are distinct.
- Reconcile unknown in-flight actions before retrying. Never claim exactly-once execution without an atomic protocol supporting it.
- Treat container, benchmark, model-provider and tool outputs as untrusted. Never put credentials in traces/artifacts.
- Compare baseline metrics before introducing complexity. Roll back changes that worsen reliability without compensating benefits.

## PR checklist
- [ ] Scope and roadmap item ID identified
- [ ] Acceptance tests and negative/security cases added
- [ ] Targeted tests and CI checks reported accurately
- [ ] Recovery, replay, idempotency and offline behavior considered
- [ ] Documentation/status updated with evidence
- [ ] No unauthorized target activity, secrets, or hidden external dependencies

## Status syntax
`[ ]` pending; `[~]` in progress (link PR); `[x]` verified complete (link commit/test); `[!]` blocked with reason. Roadmaps describe *planned* work, not proof that existing implementation is absent.
