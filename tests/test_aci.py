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
    ) == 2

    assert world.snapshot.actions["action:call-1"].status == "succeeded"
    assert world.snapshot.observations["obs:tool:call-1"].type == "tool.execution"
