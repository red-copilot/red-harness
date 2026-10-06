import json
import os
from pathlib import Path

run_dir = Path(os.environ["REDHARNESS_RUN_DIR"])
(run_dir / "proof.txt").write_text("red-harness-ok\n", encoding="utf-8")

events = [
    {"type": "session", "version": 3, "id": "pi-container", "cwd": str(run_dir)},
    {
        "type": "message_start",
        "message": {
            "role": "assistant",
            "provider": "fake",
            "model": "fake-model",
            "content": [],
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

for event in events:
    print(json.dumps(event), flush=True)
