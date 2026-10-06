import json
import sys
from pathlib import Path

from redharness.models import AgentSpec, load_task
from redharness.orchestrator import Orchestrator
from redharness.pi_adapter import PiAdapter
from redharness.trace import TraceRecorder


def _pi_spec(binary: str, launcher_args: list[str]) -> AgentSpec:
    return AgentSpec.model_validate(
        {
            "apiVersion": "redharness/v1",
            "id": "fake-pi",
            "type": "pi",
            "pi": {
                "binary": binary,
                "launcher_args": launcher_args,
                "provider": "fake",
                "model": "fake-model",
                "thinking": "medium",
            },
        }
    )


def test_pi_command_uses_safe_noninteractive_defaults(tmp_path: Path) -> None:
    spec = _pi_spec(sys.executable, [])
    adapter = PiAdapter(
        spec,
        allow_host=True,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    command = adapter._command(task, gateway_enabled=False)
    assert "--mode" in command
    assert "json" in command
    assert "--no-session" in command
    assert "--no-approve" in command
    assert "--no-context-files" in command
    assert "--no-extensions" in command
    assert "--no-skills" in command
    assert "--no-mcp" in command
    assert command[command.index("--provider") + 1] == "fake"
    assert command[command.index("--model") + 1] == "fake-model"


def test_pi_gateway_writes_isolated_compatible_provider(tmp_path: Path) -> None:
    spec = _pi_spec(sys.executable, [])
    adapter = PiAdapter(
        spec,
        allow_host=True,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run_test", "task_test"),
    )
    agent_dir = adapter._prepare_agent_dir(
        run_dir=tmp_path,
        gateway_url="http://127.0.0.1:8765",
        gateway_token="one-time-token",
    )
    models = json.loads((agent_dir / "models.json").read_text(encoding="utf-8"))
    provider = models["providers"]["redharness"]
    assert provider["baseUrl"] == "http://127.0.0.1:8765/v1"
    assert provider["api"] == "openai-completions"
    assert provider["apiKey"] == "$REDHARNESS_GATEWAY_TOKEN"
    assert provider["models"] == [{"id": "fake-model"}]


def test_pi_json_protocol_is_normalized_into_harness_metrics(tmp_path: Path) -> None:
    fake_pi = tmp_path / "fake_pi.py"
    fake_pi.write_text(
        """
import json
import os
from pathlib import Path

run_dir = Path(os.environ["REDHARNESS_RUN_DIR"])
(run_dir / "proof.txt").write_text("red-harness-ok\\n", encoding="utf-8")
records = [
    {"type": "session", "version": 3, "id": "session-1", "cwd": str(run_dir)},
    {"type": "agent_start"},
    {
        "type": "message_start",
        "message": {
            "role": "assistant",
            "provider": "fake",
            "model": "fake-model",
            "content": [],
            "stopReason": "pending",
        },
    },
    {
        "type": "tool_execution_start",
        "toolCallId": "call-1",
        "toolName": "write",
        "args": {"path": "proof.txt"},
    },
    {
        "type": "tool_execution_end",
        "toolCallId": "call-1",
        "toolName": "write",
        "result": {},
        "isError": False,
    },
    {
        "type": "message_end",
        "message": {
            "role": "assistant",
            "provider": "fake",
            "model": "fake-model",
            "content": [{"type": "text", "text": "done"}],
            "stopReason": "stop",
            "usage": {
                "input": 10,
                "output": 5,
                "cacheRead": 2,
                "cacheWrite": 3,
                "reasoning": 1,
                "totalTokens": 20,
                "cost": {"total": 0.0125},
            },
        },
    },
    {"type": "agent_settled"},
]
for record in records:
    print(json.dumps(record), flush=True)
""".strip()
        + "\n",
        encoding="utf-8",
    )
    agent_path = tmp_path / "pi-agent.yaml"
    agent_path.write_text(
        f"""apiVersion: redharness/v1
id: fake-pi
type: pi
pi:
  binary: {json.dumps(sys.executable)}
  launcher_args:
    - {json.dumps(str(fake_pi))}
  provider: fake
  model: fake-model
  thinking: medium
""",
        encoding="utf-8",
    )
    task_path = Path("benchmarks/examples/hello/task.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=_pi_spec(sys.executable, [str(fake_pi)]),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=17,
    )

    assert result["status"] == "finished"
    assert result["success"] is True
    assert result["metrics"]["model_calls"] == 1
    assert result["metrics"]["tool_calls"] == 1
    assert result["metrics"]["input_tokens"] == 10
    assert result["metrics"]["output_tokens"] == 5
    assert result["metrics"]["total_tokens"] == 20
    assert result["metrics"]["cost_usd"] == 0.0125

    run_dir = next((tmp_path / "runs").iterdir())
    trace = [
        json.loads(line)
        for line in (run_dir / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    types = [event["type"] for event in trace]
    assert "pi.session" in types
    assert "pi.agent_settled" in types
    assert "model.request" in types
    assert "model.usage" in types
    assert "tool.call" in types
    assert "tool.result" in types
