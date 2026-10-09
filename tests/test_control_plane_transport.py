from pathlib import Path

from typer.testing import CliRunner

from harness.cli import app


def test_serve_requires_tls_for_non_loopback_listener(tmp_path: Path) -> None:
    result = CliRunner().invoke(app, ["serve", "--host", "0.0.0.0"])

    assert result.exit_code != 0
    assert "non-loopback control-plane listeners" in result.output


def test_serve_passes_tls_files_to_uvicorn(tmp_path: Path, monkeypatch) -> None:
    cert = tmp_path / "server.crt"
    key = tmp_path / "server.key"
    cert.write_text("certificate", encoding="utf-8")
    key.write_text("private-key", encoding="utf-8")
    captured: dict[str, object] = {}
    monkeypatch.setenv("HARNESS_CONTROL_TOKEN", "control-token")
    monkeypatch.setattr(
        "harness.cli.uvicorn.run",
        lambda app, **kwargs: captured.update(kwargs),
    )

    result = CliRunner().invoke(
        app,
        [
            "serve",
            "--host",
            "0.0.0.0",
            "--ssl-certfile",
            str(cert),
            "--ssl-keyfile",
            str(key),
            "--queue-db",
            str(tmp_path / "control.db"),
            "--runs-root",
            str(tmp_path / "runs"),
            "--benchmarks-root",
            str(tmp_path / "benchmarks"),
            "--skills-root",
            str(tmp_path / "skills"),
        ],
    )

    assert result.exit_code == 0, result.output
    assert captured["host"] == "0.0.0.0"
    assert captured["ssl_certfile"] == str(cert)
    assert captured["ssl_keyfile"] == str(key)
