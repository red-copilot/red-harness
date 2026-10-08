"""Lightweight module boundary checks for the modular monolith.

Unlike import-time assertions, these checks do not execute application modules
or require runtime infrastructure. They inspect only imports between package
modules, including imports nested inside functions.
"""
from __future__ import annotations

import ast
from pathlib import Path


PACKAGE_ROOT = Path(__file__).resolve().parents[1] / "src" / "harness"

# Directional rules, not a prohibition on deliberate application composition.
FORBIDDEN = {
    "world": {"api", "benchmark", "runtime", "orchestrator", "cli", "worker"},
    "runtime": {"api", "benchmark", "orchestrator", "cli", "worker"},
    "benchmark": {"api", "cli", "worker"},
    "api": {"cli"},
}


def _module_parts(path: Path) -> tuple[str, ...]:
    relative = path.relative_to(PACKAGE_ROOT)
    return ("harness", *relative.with_suffix("").parts)


def _imports(path: Path) -> set[str]:
    module = _module_parts(path)
    package = module[:-1] if path.name != "__init__.py" else module[:-1]
    imports: set[str] = set()
    for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"), filename=str(path))):
        if isinstance(node, ast.Import):
            imports.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                anchor = package[:len(package) - (node.level - 1)]
                absolute = ".".join((*anchor, *(node.module or "").split(".")))
            else:
                absolute = node.module or ""
            imports.add(absolute)
            # from harness import api / from . import api
            imports.update(f"{absolute}.{alias.name}" for alias in node.names)
    return imports


def test_internal_module_dependency_direction() -> None:
    violations = []
    for root, forbidden in FORBIDDEN.items():
        base = PACKAGE_ROOT / root
        for source in base.rglob("*.py"):
            for imported in _imports(source):
                parts = imported.split(".")
                if len(parts) < 2 or parts[0] != "harness":
                    continue
                if parts[1] in forbidden:
                    violations.append(f"{source.relative_to(PACKAGE_ROOT)} -> {imported}")
    assert not violations, "invalid module dependencies:\n" + "\n".join(violations)
