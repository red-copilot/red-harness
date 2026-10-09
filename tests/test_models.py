from pathlib import Path

import pytest
from pydantic import ValidationError

from harness.models import AgentSpec, TaskSpec, load_agent, load_suite, load_task


def test_example_contracts_are_valid() -> None:
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    docker_verifier = load_task(
        Path("benchmarks/examples/hello-docker-verifier/task.yaml")
    )
    agent = load_agent(Path("agents/examples/demo.yaml"))
    pi_agent = load_agent(Path("agents/examples/pi.yaml"))
    suite = load_suite(Path("benchmarks/examples/smoke-suite.yaml"))
    assert task.api_version == "harness/v1"
    assert task.budgets.wall_time == 30
    assert docker_verifier.verification.type == "docker"
    assert docker_verifier.verification.network == "none"
    assert agent.type == "cli"
    assert pi_agent.type == "pi"
    assert pi_agent.image == "harness-pi-kali:latest"
    assert pi_agent.pi is not None
    assert pi_agent.pi.model == "gpt-5.6-sol"
    assert pi_agent.pi.cap_add == ["NET_RAW"]
    assert suite.repeat == 2


def test_docker_agent_requires_image() -> None:
    with pytest.raises(ValidationError):
        AgentSpec.model_validate(
            {
                "apiVersion": "harness/v1",
                "id": "bad-docker",
                "type": "docker",
            }
        )


def test_pi_agent_requires_pi_config_and_image() -> None:
    with pytest.raises(ValidationError):
        AgentSpec.model_validate(
            {
                "apiVersion": "harness/v1",
                "id": "bad-pi",
                "type": "pi",
            }
        )
    with pytest.raises(ValidationError):
        AgentSpec.model_validate(
            {
                "apiVersion": "harness/v1",
                "id": "bad-pi-image",
                "type": "pi",
                "pi": {"model": "m"},
            }
        )


def test_pi_accepts_container_runtime() -> None:
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1",
            "id": "pi-runsc",
            "type": "pi",
            "image": "pi:test",
            "runtime": "runsc",
            "pi": {"model": "m"},
        }
    )
    assert spec.runtime == "runsc"
    assert spec.pi is not None
    assert spec.pi.cap_add == []


@pytest.mark.parametrize(
    "agent_data",
    [
        {
            "apiVersion": "harness/v1", "id": "container-agent", "type": "docker",
            "image": "agent:test", "env": {"BENCHMARK_TOKEN": "leak"},
        },
        {
            "apiVersion": "harness/v1", "id": "pi-agent", "type": "pi",
            "image": "pi:test", "pi": {"model": "m", "env_passthrough": ["BENCHMARK_TOKEN"]},
        },
        {
            "apiVersion": "harness/v1", "id": "pi-agent", "type": "pi",
            "image": "pi:test", "pi": {"model": "m", "env_passthrough": ["HARNESS_CONTROL_TOKEN"]},
        },
        {
            "apiVersion": "harness/v1", "id": "pi-agent", "type": "pi",
            "image": "pi:test", "env": {"HARNESS_MODEL_API_KEY": "leak"},
            "pi": {"model": "m"},
        },
        {
            "apiVersion": "harness/v1", "id": "pi-agent", "type": "pi",
            "image": "pi:test", "pi": {"model": "m", "env_passthrough": ["CUSTOM_SERVICE_TOKEN"]},
        },
    ],
)
def test_container_agents_cannot_passthrough_harness_or_benchmark_credentials(agent_data) -> None:
    with pytest.raises(ValidationError, match="token-named or reserved credential"):
        AgentSpec.model_validate(agent_data)


def test_pi_can_passthrough_explicit_model_provider_api_key() -> None:
    spec = AgentSpec.model_validate(
        {
            "apiVersion": "harness/v1", "id": "pi-agent", "type": "pi",
            "image": "pi:test", "pi": {"model": "m", "env_passthrough": ["OPENAI_API_KEY"]},
        }
    )

    assert spec.pi is not None
    assert spec.pi.env_passthrough == ["OPENAI_API_KEY"]


def test_docker_verifier_requires_image() -> None:
    with pytest.raises(ValidationError):
        TaskSpec.model_validate(
            {
                "apiVersion": "harness/v1",
                "id": "bad-verifier",
                "name": "Bad verifier",
                "objective": {"description": "test"},
                "verification": {"type": "docker"},
            }
        )
