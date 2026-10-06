from pathlib import Path


def test_pi_kali_image_contains_required_base_and_tools() -> None:
    dockerfile = Path("docker/pi-kali/Dockerfile").read_text(encoding="utf-8")
    assert "FROM kalilinux/kali-rolling" in dockerfile
    assert "kali-linux-core" in dockerfile
    assert "@earendil-works/pi-coding-agent" in dockerfile
    assert "nmap" in dockerfile
    assert "sqlmap" in dockerfile
    assert "gdb" in dockerfile
