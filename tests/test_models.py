from pathlib import Path

import pytest
from pydantic import ValidationError

from redharness.models import AgentSpec, TaskSpec, load_agent, load_suite, load_task


def test_example_contracts_are_valid() -> None:
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    docker_verifier = load_task(
        Path("benchmarks/examples/hello-docker-verifier/task.yaml")
    )
    agent = load_agent(Path("agents/examples/demo.yaml"))
    pi_agent = load_agent(Path("agents/examples/pi.yaml"))
    suite = load_suite(Path("benchmarks/examples/smoke-suite.yaml"))
    assert task.api_version == "redharness/v1"
    assert task.budgets.wall_time == 30
    assert docker_verifier.verification.type == "docker"
    assert docker_verifier.verification.network == "none"
    assert agent.type == "cli"
    assert pi_agent.type == "pi"
    assert pi_agent.pi is not None
    assert pi_agent.pi.model == "gpt-5.6-sol"
    assert suite.repeat == 2


def test_docker_agent_requires_image() -> None:
    with pytest.raises(ValidationError):
        AgentSpec.model_validate(
            {
                "apiVersion": "redharness/v1",
                "id": "bad-docker",
                "type": "docker",
            }
        )


def test_pi_agent_requires_pi_config() -> None:
    with pytest.raises(ValidationError):
        AgentSpec.model_validate(
            {
                "apiVersion": "redharness/v1",
                "id": "bad-pi",
                "type": "pi",
            }
        )


def test_docker_verifier_requires_image() -> None:
    with pytest.raises(ValidationError):
        TaskSpec.model_validate(
            {
                "apiVersion": "redharness/v1",
                "id": "bad-verifier",
                "name": "Bad verifier",
                "objective": {"description": "test"},
                "verification": {"type": "docker"},
            }
        )
