from __future__ import annotations

import os
import typing
from dataclasses import dataclass


class TSecAdapterError(RuntimeError):
    pass


@dataclass(frozen=True)
class TSecConfig:
    base_url: str
    token: str


def load_tsec_config(
    *,
    base_url_env: str = "BENCHMARK_BASE_URL",
    token_env: str = "BENCHMARK_TOKEN",
) -> TSecConfig:
    base_url = os.environ.get(base_url_env, "")
    token = os.environ.get(token_env, "")
    if not base_url:
        raise TSecAdapterError(f"missing {base_url_env}")
    if not token:
        raise TSecAdapterError(f"missing {token_env}")
    return TSecConfig(base_url=base_url, token=token)


class TSecClientAdapter:
    """Thin async wrapper over the official TSec Benchmark SDK."""

    def __init__(self, config: TSecConfig) -> None:
        try:
            from tsec_benchmark import TSecBenchmarkAsync
        except ImportError as exc:
            raise TSecAdapterError(
                'tsec-benchmark is not installed; install red-harness with ".[tsec]"'
            ) from exc
        self._client = TSecBenchmarkAsync(base_url=config.base_url, token=config.token)

    async def __aenter__(self) -> typing.Self:
        await self._client.__aenter__()
        return self

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self._client.__aexit__(exc_type, exc, tb)

    async def list_challenges(self):
        return await self._client.list_challenges()

    async def start_challenge(self, unique_code: str):
        return await self._client.start_challenge(unique_code)

    async def submit_flag(self, unique_code: str, flag: str):
        return await self._client.submit_flag(unique_code, flag)

    async def close_challenge(self, unique_code: str):
        return await self._client.close_challenge(unique_code)

    async def get_hint(self, unique_code: str):
        return await self._client.get_hint(unique_code)
