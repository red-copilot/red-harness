from pathlib import Path

import pytest

from redharness.execution import FirecrackerProfile


def test_firecracker_profile_builds_api_config(tmp_path: Path) -> None:
    kernel = tmp_path / "vmlinux"
    rootfs = tmp_path / "rootfs.ext4"
    kernel.write_bytes(b"kernel")
    rootfs.write_bytes(b"rootfs")

    config = FirecrackerProfile(
        kernel_image=kernel,
        rootfs_image=rootfs,
        vcpu_count=2,
        mem_mib=512,
    ).api_config()

    assert config["machine-config"]["vcpu_count"] == 2
    assert config["machine-config"]["mem_size_mib"] == 512
    assert config["drives"][0]["is_root_device"] is True


def test_firecracker_profile_requires_images(tmp_path: Path) -> None:
    profile = FirecrackerProfile(
        kernel_image=tmp_path / "missing-kernel",
        rootfs_image=tmp_path / "missing-rootfs",
    )
    with pytest.raises(FileNotFoundError):
        profile.api_config()
