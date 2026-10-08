# EXE — Agents, tools, containers and gateway

**Scope:** `agent.py`, `session.py`, `pi_adapter.py`, `pi_container.py`, `gateway.py`, `gateway_runtime.py`, `environment.py`, `docker/pi-kali/`. Pi runs within Kali Rolling containers with long-lived RPC where configured.

### EXE-01 [ ] P0 — Execution trust boundary
Deliver: documented threat model and enforceable mounts, secret isolation, rootfs permissions, capability/PID/CPU/RAM limits, network rules, non-privileged default and artifact handoff. Agent must not access authoritative World DB, verifier or benchmark credentials.
Accept: integration tests attempt to mutate protected state, obtain secrets, escape allowed workspace and contact denied destinations; failures are observable.

### EXE-02 [ ] P1 — Adapter/session contract and resource budget
Deliver: normalized event IDs, settled/terminal semantics, tool-call correlation and per-run token/tool/wall-clock/cost accounting; avoid double charging through gateway and Pi simultaneously.
Accept: fake Pi and real Docker smoke tests cover malformed RPC stream, settle/retry, cancellation, model errors, gateway disconnect and budget exhaustion.

### EXE-03 [ ] P1 — Offline profiles and deterministic build
Deliver: separate `target-only`, `model-allowed` and `fully-offline` networking; pin image dependencies, pre-install binaries and reference data; no package installation required while solving.
Accept: no-public-egress smoke suite in a clean runner, plus network policy tests and documented model endpoint choices.

### EXE-04 [ ] P2 — Tool execution capabilities
Deliver: domain-agnostic ToolAdapter with structured execution metadata, cancellation and evidence refs, selective domain plugins; maintain approval/authorization boundaries.
Accept: contract tests across filesystem, local process and authorized network tools; outputs remain untrusted.

**Non-goals:** arbitrary unsandboxed host execution as a default; dynamic tool downloads in offline competition runs.
