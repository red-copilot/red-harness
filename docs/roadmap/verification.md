# VER — Verification and evidence

**Scope:** `action_verifier.py`, `verifier.py`, `audit.py`, `solver_loop.py`, World provenance. Existing code conservatively treats Agent observations as claims and accepts structured trusted evidence; preserve those safeguards.

### VER-01 [ ] P0 — Typed evidence and verdict contracts
Deliver: separate immutable EvidenceRef, ActionVerdict, ObjectiveVerdict models with action/run IDs, producer, capture time, world revision and artifact hash/ref; distinguish command execution, observed effect and objective completion. Reject non-Harness producers from the trusted channel. Wire adapter results without trusting an Agent-authored source string.
Accept: unit tests for forged producer, missing action ID, stale revision, conflicting evidence, failed tool with apparent success text; no false verified verdicts. Version or migrate existing event schemas.

### VER-02 [ ] P0 — Trusted verification boundary
Deliver: verifier registry and explicit authority for `tool_adapter`, isolated task verifier, benchmark evaluator; enforce producer identity at integration boundary rather than caller-supplied dictionary alone. Keep objective verdict independent of tool result.
Accept: integration test with malicious agent claims and successful command exits failing goal verification; all terminal verified goals tied to durable evidence IDs.

### VER-03 [ ] P1 — Durable evidence lifecycle and audit
Deliver: content-addressed artifact references, provenance and redaction; revision-aware invalidation/supersession; reconcile contradictory verdicts and trace links.
Accept: audit flags missing evidence, mismatched verdicts and hash corruption; event replay yields same verdict history; no secrets persisted.

### VER-04 [ ] P2 — Online/offline verifier parity
Deliver: same verifier protocol for local tasks and benchmark adapters, sandboxed verifier execution where required.
Accept: shared contract tests for success, failure, unavailable verifier, timeout and inconsistent evaluator responses; fail closed where verification is unavailable.

**Not in scope:** LLM self-confidence as proof, automatically elevating World claims, or benchmark-specific shortcuts in core.
