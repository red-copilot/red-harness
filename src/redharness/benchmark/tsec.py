from __future__ import annotations

import json
import re
from pathlib import Path

from ..agent import AgentResult
from ..models import AgentSpec, BudgetSpec
from .base import Submission
from .runner import BenchmarkRunner
from .tsec_adapter import TSecBenchmarkAdapter
from .tsec_client import (
    TSecAdapterError,
    TSecClientAdapter,
    TSecConfig,
    load_tsec_config,
)


_FLAG_RE = re.compile(r"REDHARNESS_FLAG=([^\s]+)")


def _is_sdk_error(exc: Exception, name: str) -> bool:
    return type(exc).__name__ == name


def extract_flags(raw_jsonl: str) -> list[str]:
    """Compatibility extractor for Pi JSONL assistant output."""

    flags: list[str] = []
    seen: set[str] = set()
    for line in raw_jsonl.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict):
            continue
        message = event.get("message")
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        texts: list[str] = []
        if isinstance(content, str):
            texts.append(content)
        elif isinstance(content, list):
            for item in content:
                if isinstance(item, dict) and isinstance(item.get("text"), str):
                    texts.append(item["text"])
        for text in texts:
            for match in _FLAG_RE.finditer(text):
                flag = match.group(1)
                if flag not in seen:
                    seen.add(flag)
                    flags.append(flag)
    return flags


def _extract_submissions(result: AgentResult) -> list[Submission]:
    values = extract_flags(result.stdout)
    if not values:
        values = [
            match.group(1)
            for match in _FLAG_RE.finditer(result.stdout)
        ]
    seen: set[str] = set()
    submissions: list[Submission] = []
    for value in values:
        if value in seen:
            continue
        seen.add(value)
        submissions.append(Submission(type="flag", value=value))
    return submissions


class TSecRunner:
    """Compatibility facade over the generic BenchmarkRunner."""

    def __init__(self, *, runs_root: str | Path) -> None:
        self.runs_root = Path(runs_root)

    async def run(
        self,
        *,
        config: TSecConfig,
        agent: AgentSpec,
        budgets: BudgetSpec,
        challenge_code: str | None = None,
        run_all: bool = False,
        seed: int = 0,
        use_hint: bool = False,
        start_retries: int = 2,
        retry_delay: float = 2.0,
        allow_host_agent: bool = False,
    ) -> list[dict]:
        results: list[dict] = []
        try:
            async with TSecClientAdapter(config) as client:
                adapter = TSecBenchmarkAdapter(
                    client,
                    use_hint=use_hint,
                    start_retries=start_retries,
                    retry_delay=retry_delay,
                )
                cases = await adapter.discover()
                if challenge_code and not any(case.id == challenge_code for case in cases):
                    raise TSecAdapterError(f"challenge not found: {challenge_code}")

                selected = [
                    case
                    for case in cases
                    if not bool(case.metadata.get("is_completed"))
                    and (
                        run_all
                        or challenge_code is None
                        or case.id == challenge_code
                    )
                ]
                if not run_all and challenge_code is None:
                    selected = selected[:1]

                runner = BenchmarkRunner(runs_root=self.runs_root)
                for index, case in enumerate(selected):
                    results.append(
                        await runner.run_case(
                            adapter=adapter,
                            case=case,
                            agent=agent,
                            budgets=budgets,
                            seed=seed + index,
                            submission_extractor=_extract_submissions,
                            allow_host_agent=allow_host_agent,
                        )
                    )
        except Exception as exc:
            if _is_sdk_error(exc, "VpnCheckError"):
                reason = getattr(exc, "detail", None)
                raise TSecAdapterError(
                    f"TSec VPN preflight failed: {getattr(exc, 'message', str(exc))}; "
                    f"detail={reason}"
                ) from exc
            raise
        return results


__all__ = [
    "TSecAdapterError",
    "TSecClientAdapter",
    "TSecConfig",
    "TSecRunner",
    "extract_flags",
    "load_tsec_config",
]
