from __future__ import annotations

import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class ExecutionCapabilities:
    docker: bool
    gvisor_runsc: bool
    firecracker: bool
    kvm: bool

    @classmethod
    def detect(cls) -> ExecutionCapabilities:
        return cls(
            docker=shutil.which("docker") is not None,
            gvisor_runsc=shutil.which("runsc") is not None,
            firecracker=shutil.which("firecracker") is not None,
            kvm=Path("/dev/kvm").exists(),
        )

    def as_dict(self) -> dict[str, bool]:
        return {
            "docker": self.docker,
            "gvisor_runsc": self.gvisor_runsc,
            "firecracker": self.firecracker,
            "kvm": self.kvm,
        }


@dataclass(frozen=True)
class FirecrackerProfile:
    kernel_image: Path
    rootfs_image: Path
    vcpu_count: int = 2
    mem_mib: int = 2048
    boot_args: str = "console=ttyS0 reboot=k panic=1 pci=off"

    def validate(self) -> None:
        if self.vcpu_count < 1:
            raise ValueError("vcpu_count must be >= 1")
        if self.mem_mib < 128:
            raise ValueError("mem_mib must be >= 128")
        if not self.kernel_image.is_file():
            raise FileNotFoundError(self.kernel_image)
        if not self.rootfs_image.is_file():
            raise FileNotFoundError(self.rootfs_image)

    def api_config(self) -> dict[str, Any]:
        self.validate()
        return {
            "boot-source": {
                "kernel_image_path": str(self.kernel_image.resolve()),
                "boot_args": self.boot_args,
            },
            "drives": [
                {
                    "drive_id": "rootfs",
                    "path_on_host": str(self.rootfs_image.resolve()),
                    "is_root_device": True,
                    "is_read_only": False,
                }
            ],
            "machine-config": {
                "vcpu_count": self.vcpu_count,
                "mem_size_mib": self.mem_mib,
                "smt": False,
            },
        }


class FirecrackerBackend:
    """Firecracker host contract.

    v0.6 intentionally exposes capability/config validation without pretending CI has
    KVM. VM lifecycle wiring will use this contract once a KVM-enabled worker exists.
    """

    @staticmethod
    def available() -> bool:
        caps = ExecutionCapabilities.detect()
        return caps.firecracker and caps.kvm

    @staticmethod
    def validate_host() -> None:
        caps = ExecutionCapabilities.detect()
        if not caps.firecracker:
            raise RuntimeError("firecracker executable was not found")
        if not caps.kvm:
            raise RuntimeError("/dev/kvm is unavailable")
