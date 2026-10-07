from __future__ import annotations

from .models import (
    ActionRecord,
    Artifact,
    Capability,
    Constraint,
    Entity,
    Failure,
    Goal,
    Hypothesis,
    Observation,
    Relation,
    WorldEvent,
    WorldSnapshot,
)

_KIND_MAP = {
    "entity": ("entities", Entity),
    "relation": ("relations", Relation),
    "observation": ("observations", Observation),
    "artifact": ("artifacts", Artifact),
    "capability": ("capabilities", Capability),
    "hypothesis": ("hypotheses", Hypothesis),
    "goal": ("goals", Goal),
    "action": ("actions", ActionRecord),
    "constraint": ("constraints", Constraint),
    "failure": ("failures", Failure),
}


class WorldReducer:
    def apply(self, snapshot: WorldSnapshot, event: WorldEvent) -> WorldSnapshot:
        collection_name, model_type = _KIND_MAP[event.kind]
        collection = getattr(snapshot, collection_name)
        object_id = event.object.get("id")
        if not isinstance(object_id, str) or not object_id:
            raise ValueError("world events require object.id")

        if event.op == "remove":
            collection.pop(object_id, None)
        else:
            payload = dict(event.object)
            provenance = dict(payload.get("provenance") or {})
            provenance["actor"] = event.actor
            provenance["event_id"] = event.id
            if event.source_event_id is not None:
                provenance["source_event_id"] = event.source_event_id
            payload["provenance"] = provenance
            collection[object_id] = model_type.model_validate(payload)

        snapshot.revision += 1
        return snapshot

    def replay(self, events: list[WorldEvent]) -> WorldSnapshot:
        snapshot = WorldSnapshot()
        for event in events:
            self.apply(snapshot, event)
        return snapshot
