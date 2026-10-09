import asyncio
from pathlib import Path

import pytest

from harness.tool_adapter import ToolExecutionContext, ToolExecutionResult, execute_tool_adapter


def test_adapter_receives_cancellation_before_its_task_is_stopped(tmp_path: Path) -> None:
    async def scenario() -> None:
        started = asyncio.Event()
        observed_cancel = asyncio.Event()

        class WaitingAdapter:
            tool_name = "test.wait"

            async def execute(self, arguments, context):
                started.set()
                await context.cancelled.wait()
                observed_cancel.set()
                return ToolExecutionResult(output={"cancelled": True})

        context = ToolExecutionContext(
            call_id="call-1",
            workspace=tmp_path,
            task_dir=tmp_path,
            cancelled=asyncio.Event(),
        )
        operation = asyncio.create_task(execute_tool_adapter(WaitingAdapter(), {}, context))
        await started.wait()
        operation.cancel()

        with pytest.raises(asyncio.CancelledError):
            await operation

        assert observed_cancel.is_set()
        assert context.cancelled.is_set()

    asyncio.run(scenario())
