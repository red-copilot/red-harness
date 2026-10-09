from pathlib import Path


def test_pi_kali_image_contains_required_base_and_tools() -> None:
    dockerfile = Path("docker/pi-kali/Dockerfile").read_text(encoding="utf-8")
    assert "kalilinux/kali-last-release@sha256:" in dockerfile
    assert "kali-linux-core" in dockerfile
    assert "@earendil-works/pi-coding-agent" in dockerfile
    assert "ARG PI_VERSION=1.0.4" in dockerfile
    assert "kali-last-snapshot" in dockerfile
    assert "s/kali-rolling/kali-last-snapshot/g" in dockerfile
    assert dockerfile.index("kali-last-snapshot") < dockerfile.index("apt-get update")
    assert "apt-get update" in dockerfile
    assert "nmap" in dockerfile
    assert "sqlmap" in dockerfile
    assert "gdb" in dockerfile
