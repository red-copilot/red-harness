# Modular monolith boundaries

Harness is one deployable Python application, not a collection of microservices.
Modules are separated by responsibilities and explicit composition points.

- `runtime/`: application-level composition, solver initialization, run state.
- `world/`: domain state, event storage, snapshots, retrieval. No benchmark or HTTP dependencies.
- `benchmark/`: externally provisioned challenge lifecycle, submissions and adapters.
- `api/`: transport-facing HTTP handlers, not a place for solver policies.
- `agent.py`, `session.py`, `pi_*.py`: agent/session integration and infrastructure.
- `solver_loop.py`, `planner.py`, `action_verifier.py`: solver policies.
- `orchestrator.py`: local-run lifecycle and environment integration.

The first extraction is `runtime.bootstrap_solver`. Both local runs and
externally provisioned benchmarks create the same solver state through this
composition point. Runtime does not provision environments, contact the
benchmark SDK, or run verifiers.

Dependency direction (initial migration):

```text
CLI / HTTP / worker
       | 
local orchestrator       benchmark runner
          \                /
           runtime bootstrap
              |   |   |
         solver world progress
```

New modules must not import the CLI, HTTP API or benchmark-specific adapters
from the domain model. Avoid exposing database connection objects across
module boundaries. Preserve public import paths until migration is complete.

This is an incremental refactor: the older top-level modules remain valid,
and this document does not claim the full package has been relocated.

## Enforced dependency direction

`tests/test_architecture.py` parses package imports without importing modules.
It rejects reverse imports from world into API, benchmark, runtime or entry
points; from runtime into transport or use-case entry points; and from
benchmark adapters into API/CLI/worker. These rules deliberately target
high-risk dependency inversions instead of pretending all top-level modules
have already been migrated.

CI executes the boundary check in the regular pytest suite. A boundary
exception should be explicit, documented and reviewed rather than hidden
behind runtime import aliases.

## Planned migration sequence

1. Extract shared bootstrapping (completed in this branch).
2. Introduce import-boundary tests (completed in this branch).
3. Move run-result persistence/serialization behind a runtime service (done),
   then unify local and external benchmark lifecycle states.
4. Separate SDK/network, Docker and Pi infrastructure from policy logic.
5. Consolidate composition at CLI and worker entry points, preserving
   existing external interfaces and world event format.
