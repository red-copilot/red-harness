from __future__ import annotations

import json
import re
from collections.abc import Iterable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from .models import WorldSnapshot


_TOKEN_RE = re.compile(r"[A-Za-z0-9_.:/-]{3,}")


@dataclass(frozen=True)
class RetrievedRecord:
    kind: str
    item: Any
    score: float


class WorldRetriever:
    """Deterministic goal-aware retrieval over the symbolic WorldSnapshot."""

    KIND_WEIGHTS = {
        "goals": 5.0,
        "constraints": 4.0,
        "capabilities": 3.5,
        "hypotheses": 3.0,
        "failures": 2.5,
        "observations": 2.0,
        "entities": 2.0,
        "actions": 1.5,
        "artifacts": 1.5,
        "relations": 1.0,
    }

    DEFAULT_LIMITS = {
        "goals": 8,
        "constraints": 12,
        "capabilities": 16,
        "hypotheses": 12,
        "failures": 10,
        "observations": 24,
        "entities": 20,
        "actions": 16,
        "artifacts": 16,
        "relations": 20,
    }

    def __init__(self, *, limits: dict[str, int] | None = None) -> None:
        self.limits = {**self.DEFAULT_LIMITS, **(limits or {})}

    @staticmethod
    def _tokens(value: str) -> set[str]:
        return {token.lower() for token in _TOKEN_RE.findall(value)}

    @classmethod
    def _query_text(cls, snapshot: WorldSnapshot, query: str | None) -> str:
        if query and query.strip():
            return query
        active = [
            goal.description
            for goal in snapshot.goals.values()
            if goal.status in {"active", "pending"} and goal.is_valid_at()
        ]
        return "\n".join(active)

    @staticmethod
    def _serialized(item: Any) -> str:
        return json.dumps(
            item.model_dump(mode="json", exclude_none=True),
            ensure_ascii=False,
            sort_keys=True,
            default=str,
        )

    @staticmethod
    def _recency_bonus(items: list[Any]) -> dict[str, float]:
        dated = [
            item
            for item in items
            if getattr(item, "observed_at", None) is not None
        ]
        dated.sort(
            key=lambda item: getattr(item, "observed_at", datetime.min.replace(tzinfo=UTC)),
            reverse=True,
        )
        if not dated:
            return {}
        denominator = max(1, len(dated) - 1)
        return {
            item.id: 1.0 - (index / denominator)
            for index, item in enumerate(dated)
        }

    def _score(
        self,
        *,
        kind: str,
        item: Any,
        query_tokens: set[str],
        recency: dict[str, float],
    ) -> float:
        text_tokens = self._tokens(self._serialized(item))
        overlap = len(query_tokens & text_tokens)
        lexical = overlap / max(1, len(query_tokens))

        confidence = getattr(item, "confidence", None)
        confidence_bonus = float(confidence) if confidence is not None else 0.0

        status = getattr(item, "status", None)
        status_bonus = 0.5 if status in {"active", "open", "supported", "running"} else 0.0

        return (
            self.KIND_WEIGHTS.get(kind, 1.0)
            + 5.0 * lexical
            + 0.75 * confidence_bonus
            + 0.5 * recency.get(item.id, 0.0)
            + status_bonus
        )

    def retrieve(
        self,
        snapshot: WorldSnapshot,
        *,
        query: str | None = None,
    ) -> dict[str, list[Any]]:
        query_tokens = self._tokens(self._query_text(snapshot, query))
        output: dict[str, list[Any]] = {}

        for kind in self.KIND_WEIGHTS:
            collection: dict[str, Any] = getattr(snapshot, kind)
            valid = [item for item in collection.values() if item.is_valid_at()]
            recency = self._recency_bonus(valid)
            ranked = sorted(
                valid,
                key=lambda item: (
                    -self._score(
                        kind=kind,
                        item=item,
                        query_tokens=query_tokens,
                        recency=recency,
                    ),
                    item.id,
                ),
            )
            output[kind] = ranked[: self.limits.get(kind, len(ranked))]

        return output


def flatten_retrieval(records: dict[str, Iterable[Any]]) -> list[tuple[str, Any]]:
    return [
        (kind, item)
        for kind, items in records.items()
        for item in items
    ]
