from pathlib import Path

import pytest

from harness.skills import SkillSpec, load_skill, scan_skills


def test_load_skill_contract() -> None:
    skill = load_skill("skills/examples/network-service-discovery.yaml")

    assert skill.id == "network.service-discovery"
    assert skill.domain == "network"
    assert skill.requires[0].kind == "capability"
    assert skill.produces[0].kind == "observation"


def test_scan_skills_reports_invalid_files(tmp_path: Path) -> None:
    valid = tmp_path / "valid.yaml"
    valid.write_text(
        """
apiVersion: harness/skill/v1
id: demo
domain: general
description: Demo skill
requires: []
produces: []
""".strip()
        + "\n",
        encoding="utf-8",
    )
    invalid = tmp_path / "invalid.yaml"
    invalid.write_text("apiVersion: wrong\nid: broken\n", encoding="utf-8")

    results = scan_skills(tmp_path)

    assert len(results) == 2
    assert sum(1 for item in results if item["valid"]) == 1
    assert sum(1 for item in results if not item["valid"]) == 1


def test_skill_spec_is_domain_neutral() -> None:
    skill = SkillSpec(
        id="custom.technique",
        domain="custom",
        description="A domain extension can use namespaced state types.",
        requires=[{"kind": "entity", "type": "custom.asset"}],
        produces=[{"kind": "capability", "type": "custom.access"}],
    )

    assert skill.requires[0].type == "custom.asset"
    assert skill.produces[0].type == "custom.access"


def test_skill_rejects_unreviewed_top_level_execution_metadata() -> None:
    with pytest.raises(ValueError):
        SkillSpec.model_validate(
            {
                "id": "unsafe",
                "description": "unreviewed execution field",
                "command": ["bash", "-c", "touch /tmp/side-effect"],
            }
        )
