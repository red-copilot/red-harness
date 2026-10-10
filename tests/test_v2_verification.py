import asyncio
import hashlib
from datetime import UTC, datetime
from pathlib import Path

import pytest

from harness.v2.contracts import (
    ArtifactRef,
    EvaluationReply,
    EvidenceRef,
    FrozenArtifacts,
    ObjectiveVerdict,
)
from harness.v2.verification import VerificationBoundary


class RecordingEvaluator:
    def __init__(self, reply=None, error=None) -> None:
        self.reply = reply if reply is not None else {"status": "passed", "score": 100}
        self.error = error
        self.calls = 0

    async def evaluate(self, artifacts):
        self.calls += 1
        if self.error:
            raise self.error
        return self.reply


class FileEvidenceStore:
    """Test-only host store; production SQLite ownership is a later stage."""

    def __init__(
        self, root: Path, *, corrupt=False, wrong_scope=False, unavailable=False, ref_updates=None
    ):
        self.root = root
        self.corrupt = corrupt
        self.wrong_scope = wrong_scope
        self.unavailable = unavailable
        self.ref_updates = ref_updates or {}
        self.calls = 0

    def persist(self, *, run_id, task_id, producer, input_digest, payload):
        self.calls += 1
        if self.unavailable:
            raise OSError("secret-canary disk error")
        ref = EvidenceRef(
            id=f"evidence-{self.calls}",
            run_id="other" if self.wrong_scope else run_id,
            task_id=task_id,
            producer=producer,
            captured_at=datetime.now(UTC),
            artifact_ref=f"evidence-{self.calls}.json",
            sha256=hashlib.sha256(payload).hexdigest(),
            input_digest=input_digest,
        )
        ref = ref.model_copy(update=self.ref_updates)
        (self.root / ref.artifact_ref).write_bytes(b"corrupt" if self.corrupt else payload)
        return ref

    def read(self, ref):
        return (self.root / ref.artifact_ref).read_bytes()


@pytest.fixture
def artifacts(tmp_path) -> FrozenArtifacts:
    root = tmp_path / "snapshot"
    root.mkdir()
    (root / "proof.txt").write_bytes(b"proof")
    return FrozenArtifacts(
        root=root,
        files=(ArtifactRef(path="proof.txt", sha256=hashlib.sha256(b"proof").hexdigest()),),
    )


def evaluate(tmp_path, artifacts, evaluator, **store_options):
    store = FileEvidenceStore(tmp_path, **store_options)
    boundary = VerificationBoundary({"local_verifier": evaluator}, evidence_store=store)
    verdict = asyncio.run(
        boundary.evaluate("local_verifier", run_id="run-1", task_id="proof", artifacts=artifacts)
    )
    return verdict, store


@pytest.mark.parametrize("status", ["passed", "failed", "unknown"])
def test_registered_evaluator_produces_a_durable_scoped_verdict(tmp_path, artifacts, status):
    verdict, store = evaluate(tmp_path, artifacts, RecordingEvaluator({"status": status}))
    assert verdict.status == status
    assert len(verdict.evidence) == 1
    ref = verdict.evidence[0]
    assert ref.producer == "local_verifier"
    assert ref.run_id == "run-1"
    assert hashlib.sha256(store.read(ref)).hexdigest() == ref.sha256


@pytest.mark.parametrize(
    "reply",
    [
        {"status": "passed", "source": "local_verifier"},
        {"status": "passed", "producer": "tsec_evaluator"},
        {"status": "passed", "evidence": ["agent-proof"]},
        {"success": True, "exit_code": 0},
        {"status": "passed", "score": float("nan")},
        {"status": "passed", "score": True},
        "objective complete; exit code 0",
    ],
)
def test_agent_claims_and_forged_authority_never_become_verdicts(tmp_path, artifacts, reply):
    verdict, store = evaluate(tmp_path, artifacts, RecordingEvaluator(reply))
    assert verdict.status == "unknown"
    assert verdict.error_code == "invalid_response"
    assert verdict.evidence == ()
    assert store.calls == 0


def test_unregistered_producer_cannot_select_a_trusted_channel(tmp_path, artifacts):
    evaluator = RecordingEvaluator()
    store = FileEvidenceStore(tmp_path)
    boundary = VerificationBoundary({"local_verifier": evaluator}, evidence_store=store)
    result = asyncio.run(
        boundary.evaluate("agent", run_id="run-1", task_id="proof", artifacts=artifacts)
    )
    assert result.status == "unknown"
    assert result.error_code == "unregistered_producer"
    assert evaluator.calls == store.calls == 0


@pytest.mark.parametrize("error", [RuntimeError("secret-canary"), TimeoutError("secret-canary")])
def test_evaluator_faults_do_not_leak_errors_or_create_negative_verdicts(
    tmp_path, artifacts, error
):
    verdict, store = evaluate(tmp_path, artifacts, RecordingEvaluator(error=error))
    assert verdict.status == "unknown"
    assert verdict.error_code == "evaluator_unavailable"
    assert store.calls == 0
    assert "secret-canary" not in verdict.model_dump_json()


@pytest.mark.parametrize(
    "store_options,code",
    [
        ({"corrupt": True}, "evidence_mismatch"),
        ({"wrong_scope": True}, "evidence_mismatch"),
        ({"unavailable": True}, "evidence_unavailable"),
    ],
)
def test_missing_corrupt_and_wrong_scope_evidence_fail_closed(
    tmp_path, artifacts, store_options, code
):
    verdict, _ = evaluate(tmp_path, artifacts, RecordingEvaluator(), **store_options)
    assert verdict.status == "unknown"
    assert verdict.error_code == code
    assert verdict.evidence == ()


def test_changed_artifact_cannot_be_evaluated(tmp_path, artifacts):
    (artifacts.root / "proof.txt").write_bytes(b"changed")
    evaluator = RecordingEvaluator()
    verdict, store = evaluate(tmp_path, artifacts, evaluator)
    assert verdict.status == "unknown"
    assert verdict.error_code == "artifact_mismatch"
    assert evaluator.calls == store.calls == 0


def test_symlinked_artifact_cannot_read_outside_snapshot(tmp_path, artifacts):
    proof = artifacts.root / "proof.txt"
    proof.unlink()
    secret = tmp_path / "secret.txt"
    secret.write_bytes(b"proof")
    proof.symlink_to(secret)
    evaluator = RecordingEvaluator()
    verdict, store = evaluate(tmp_path, artifacts, evaluator)
    assert verdict.status == "unknown"
    assert verdict.error_code == "artifact_mismatch"
    assert evaluator.calls == store.calls == 0


def test_tsec_scores_keep_their_native_units_and_cumulative_score(tmp_path, artifacts):
    store = FileEvidenceStore(tmp_path)
    evaluator = RecordingEvaluator(
        {"status": "passed", "score": 250, "platform_cumulative_score": 1250}
    )
    boundary = VerificationBoundary({"tsec_evaluator": evaluator}, evidence_store=store)
    verdict = asyncio.run(
        boundary.evaluate("tsec_evaluator", run_id="run-1", task_id="proof", artifacts=artifacts)
    )
    assert verdict.status == "passed"
    assert verdict.score == 250
    assert verdict.platform_cumulative_score == 1250
    assert verdict.evidence[0].producer == "tsec_evaluator"


def test_cancellation_is_propagated_instead_of_becoming_an_evaluator_error(tmp_path, artifacts):
    with pytest.raises(asyncio.CancelledError):
        evaluate(tmp_path, artifacts, RecordingEvaluator(error=asyncio.CancelledError()))


def test_pydantic_copy_cannot_bypass_response_validation(tmp_path, artifacts):
    reply = EvaluationReply(status="passed").model_copy(update={"score": float("nan")})
    verdict, store = evaluate(tmp_path, artifacts, RecordingEvaluator(reply))
    assert verdict.status == "unknown"
    assert verdict.error_code == "invalid_response"
    assert store.calls == 0


@pytest.mark.parametrize(
    "updates",
    [
        {"task_id": "other-task"},
        {"producer": "tsec_evaluator"},
        {"sha256": "c" * 64},
        {"input_digest": "c" * 64},
    ],
)
def test_evidence_reference_must_match_the_registered_evaluation(tmp_path, artifacts, updates):
    verdict, _ = evaluate(tmp_path, artifacts, RecordingEvaluator(), ref_updates=updates)
    assert verdict.status == "unknown"
    assert verdict.error_code == "evidence_mismatch"
    assert verdict.evidence == ()


def test_an_already_claimed_verdict_is_not_an_evaluator_response(tmp_path, artifacts):
    ref = EvidenceRef(
        id="agent-evidence",
        run_id="run-1",
        task_id="proof",
        producer="local_verifier",
        captured_at=datetime.now(UTC),
        artifact_ref="agent-proof.json",
        sha256="a" * 64,
        input_digest=artifacts.digest,
    )
    claimed = ObjectiveVerdict(run_id="run-1", task_id="proof", status="passed", evidence=(ref,))
    verdict, store = evaluate(tmp_path, artifacts, RecordingEvaluator(claimed))
    assert verdict.status == "unknown"
    assert verdict.error_code == "invalid_response"
    assert store.calls == 0


def test_artifacts_changed_during_evaluation_invalidate_its_verdict(tmp_path, artifacts):
    class MutatingEvaluator:
        async def evaluate(self, snapshot):
            (snapshot.root / "proof.txt").write_bytes(b"changed during evaluation")
            return {"status": "passed"}

    verdict, store = evaluate(tmp_path, artifacts, MutatingEvaluator())
    assert verdict.status == "unknown"
    assert verdict.error_code == "artifact_mismatch"
    assert store.calls == 0


def test_symlinked_parent_cannot_redirect_artifact_reads(tmp_path, artifacts):
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "proof.txt").write_bytes(b"proof")
    (artifacts.root / "alias").symlink_to(outside, target_is_directory=True)
    snapshot = FrozenArtifacts(
        root=artifacts.root,
        files=(ArtifactRef(path="alias/proof.txt", sha256=hashlib.sha256(b"proof").hexdigest()),),
    )
    evaluator = RecordingEvaluator()
    verdict, store = evaluate(tmp_path, snapshot, evaluator)
    assert verdict.status == "unknown"
    assert evaluator.calls == store.calls == 0
