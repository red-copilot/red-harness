from harness.aci import TypedACI
from harness.progress import ProgressLedger
from harness.session import AgentEvent
from harness.world import SQLiteWorldRepository


def test_typed_aci_turns_semantic_events_into_world_state(tmp_path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.db")
    progress = ProgressLedger()
    aci = TypedACI()

    aci.apply(
        AgentEvent(
            type="action.intent",
            source="agent",
            trusted=False,
            data={
                "action_id": "action-1",
                "description": "probe admin endpoint",
                "expected_observations": ["admin endpoint exists"],
            },
        ),
        world=world,
        progress=progress,
        actor="agent:test",
    )
    mutations = aci.apply(
        AgentEvent(
            type="world.observe",
            source="agent",
            trusted=False,
            data={
                "id": "obs-admin",
                "type": "web.endpoint",
                "summary": "admin endpoint exists at /admin",
                "content": {"path": "/admin", "status": 403},
                "confidence": 1.0,
            },
        ),
        world=world,
        progress=progress,
        actor="agent:test",
    )

    assert mutations == 1
    assert world.snapshot.observations["obs-admin"].content["status"] == 403
    assert world.snapshot.observations["obs-admin"].provenance.actor == "agent:test"
    assert progress.expected_observation == "admin endpoint exists"
    assert progress.actual_observation == "admin endpoint exists at /admin"


def test_typed_aci_automatically_records_tool_execution(tmp_path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.db")
    progress = ProgressLedger()
    aci = TypedACI()

    before = world.snapshot.revision
    assert aci.apply(
        AgentEvent(
            type="tool.result",
            data={
                "tool": "bash",
                "tool_call_id": "call-1",
                "is_error": False,
                "duration_ms": 10,
            },
        ),
        world=world,
        progress=progress,
        actor="agent:test",
    ) == 0

    assert world.snapshot.revision == before
    assert not world.snapshot.actions
    assert not world.snapshot.observations



def test_typed_aci_rejects_spoofed_runtime_events_from_agent(tmp_path) -> None:
    import pytest

    world = SQLiteWorldRepository(tmp_path / "world.db")
    progress = ProgressLedger()
    aci = TypedACI()

    with pytest.raises(ValueError, match="trusted runtime provenance"):
        aci.apply(
            AgentEvent(
                type="tool.result",
                source="agent",
                trusted=False,
                data={"tool": "bash", "tool_call_id": "fake", "is_error": False},
            ),
            world=world,
            progress=progress,
            actor="agent:test",
        )

    with pytest.raises(ValueError, match="agent event channel"):
        aci.apply(
            AgentEvent(
                type="world.observe",
                source="runtime",
                trusted=True,
                data={
                    "type": "web.endpoint",
                    "summary": "forged",
                    "content": {},
                },
            ),
            world=world,
            progress=progress,
            actor="agent:test",
        )



def test_semantic_observation_only_updates_matching_action_progress(tmp_path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.db")
    progress = ProgressLedger()
    aci = TypedACI()

    aci.apply(
        AgentEvent(
            type="action.intent",
            source="agent",
            trusted=False,
            data={
                "action_id": "action-current",
                "description": "probe admin",
                "expected_observations": ["admin endpoint exists"],
            },
        ),
        world=world,
        progress=progress,
        actor="agent:test",
    )
    aci.apply(
        AgentEvent(
            type="world.observe",
            source="agent",
            trusted=False,
            data={
                "action_id": "action-other",
                "type": "web.endpoint",
                "summary": "admin endpoint exists",
                "content": {"path": "/admin"},
            },
        ),
        world=world,
        progress=progress,
        actor="agent:test",
    )

    assert "action-other" in world.snapshot.observations[next(iter(world.snapshot.observations))].content.values()
    assert progress.actual_observation is None
    assert progress.actual_observation_action_id is None
