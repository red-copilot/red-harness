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
FORBIDDEN_EXTERNAL = {
    "world": {"fastapi", "starlette", "docker", "tsec_benchmark", "subprocess", "httpx"},
    "runtime": {
        "fastapi", "starlette", "docker", "tsec_benchmark", "subprocess", "httpx"
    },
    "benchmark": {"fastapi", "starlette", "docker"},
}


def _module_parts(path: Path, package_root: Path = PACKAGE_ROOT) -> tuple[str, ...]:
    relative = path.relative_to(package_root)
    return ("harness", *relative.with_suffix("").parts)


def _imports(path: Path, package_root: Path = PACKAGE_ROOT) -> set[str]:
    module = _module_parts(path, package_root)
    package = module[:-1]
    tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    imports: set[str] = set()
    importlib_aliases: set[str] = set()
    builtins_aliases: set[str] = set()
    dynamic_import_aliases: set[str] = set()
    dynamic_code_aliases: set[str] = {"eval", "exec"}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name == "importlib" or alias.name.startswith("importlib."):
                    importlib_aliases.add(alias.asname or alias.name.split(".", 1)[0])
                if alias.name == "builtins":
                    builtins_aliases.add(alias.asname or "builtins")
        elif isinstance(node, ast.ImportFrom):
            if node.module == "importlib":
                dynamic_import_aliases.update(
                    alias.asname or alias.name
                    for alias in node.names
                    if alias.name == "import_module"
                )
            elif node.module == "builtins":
                dynamic_import_aliases.update(
                    alias.asname or alias.name
                    for alias in node.names
                    if alias.name == "__import__"
                )
                dynamic_code_aliases.update(
                    alias.asname or alias.name
                    for alias in node.names
                    if alias.name in {"eval", "exec"}
                )

    def assigned_names(target: ast.expr) -> set[str]:
        if isinstance(target, ast.Name):
            return {target.id}
        if isinstance(target, (ast.Tuple, ast.List)):
            return set().union(*(assigned_names(item) for item in target.elts))
        return set()

    for node in ast.walk(tree):
        if isinstance(node, ast.Assign):
            targets = set().union(*(assigned_names(target) for target in node.targets))
            value = node.value
        elif isinstance(node, ast.AnnAssign):
            targets = assigned_names(node.target)
            value = node.value
        else:
            continue
        if value is None:
            continue
        is_dynamic_import = isinstance(value, ast.Name) and (
            value.id == "__import__" or value.id in dynamic_import_aliases
        )
        is_dynamic_import = is_dynamic_import or (
            isinstance(value, ast.Attribute)
            and value.attr == "import_module"
            and isinstance(value.value, ast.Name)
            and value.value.id in importlib_aliases
        )
        is_dynamic_import = is_dynamic_import or (
            isinstance(value, ast.Attribute)
            and value.attr == "__import__"
            and isinstance(value.value, ast.Name)
            and value.value.id in builtins_aliases
        )
        is_dynamic_import = is_dynamic_import or (
            isinstance(value, ast.Call)
            and isinstance(value.func, ast.Name)
            and value.func.id == "getattr"
            and len(value.args) >= 2
            and isinstance(value.args[0], ast.Name)
            and isinstance(value.args[1], ast.Constant)
            and (
                (value.args[0].id in importlib_aliases
                 and value.args[1].value == "import_module")
                or (value.args[0].id in builtins_aliases
                    and value.args[1].value == "__import__")
            )
        )
        is_dynamic_code = isinstance(value, ast.Name) and value.id in dynamic_code_aliases
        is_dynamic_code = is_dynamic_code or (
            isinstance(value, ast.Attribute)
            and isinstance(value.value, ast.Name)
            and value.value.id in builtins_aliases
            and value.attr in {"eval", "exec"}
        )
        is_dynamic_code = is_dynamic_code or (
            isinstance(value, ast.Call)
            and isinstance(value.func, ast.Name)
            and value.func.id == "getattr"
            and len(value.args) >= 2
            and isinstance(value.args[0], ast.Name)
            and value.args[0].id in builtins_aliases
            and isinstance(value.args[1], ast.Constant)
            and value.args[1].value in {"eval", "exec"}
        )
        if is_dynamic_code:
            dynamic_code_aliases.update(targets)
        if is_dynamic_import:
            dynamic_import_aliases.update(targets)

    def is_dynamic_import_callable(node: ast.AST) -> bool:
        if isinstance(node, ast.Name):
            return node.id == "__import__" or node.id in dynamic_import_aliases
        if isinstance(node, ast.Attribute):
            if isinstance(node.value, ast.Name):
                return (
                    node.attr == "import_module" and node.value.id in importlib_aliases
                ) or (node.attr == "__import__" and node.value.id in builtins_aliases)
            if isinstance(node.value, ast.Call) and is_dynamic_import_callable(node.value.func):
                imported_module = (
                    _static_string(node.value.args[0]) if node.value.args else None
                )
                return (node.attr == "import_module" and imported_module == "importlib") or (
                    node.attr == "__import__" and imported_module == "builtins"
                )
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            if node.func.id != "getattr" or len(node.args) < 2:
                return False
            owner, attribute = node.args[:2]
            if not isinstance(owner, ast.Name) or not isinstance(attribute, ast.Constant):
                return False
            return (
                owner.id in importlib_aliases and attribute.value == "import_module"
            ) or (owner.id in builtins_aliases and attribute.value == "__import__")
        return False

    def is_dynamic_code_callable(node: ast.AST) -> bool:
        if isinstance(node, ast.Name):
            return node.id in dynamic_code_aliases
        if isinstance(node, ast.Attribute):
            return (
                isinstance(node.value, ast.Name)
                and node.value.id in builtins_aliases
                and node.attr in {"eval", "exec"}
            )
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            return (
                node.func.id == "getattr"
                and len(node.args) >= 2
                and isinstance(node.args[0], ast.Name)
                and node.args[0].id in builtins_aliases
                and isinstance(node.args[1], ast.Constant)
                and node.args[1].value in {"eval", "exec"}
            )
        return False

    for node in ast.walk(tree):
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
        elif isinstance(node, ast.Call) and is_dynamic_import_callable(node.func):
            target = _static_string(node.args[0]) if node.args else None
            imports.add(target if target is not None else "<dynamic import target>")
        elif isinstance(node, ast.Call) and is_dynamic_code_callable(node.func):
            imports.add("<dynamic code execution>")
    return imports


def _static_string(node: ast.AST) -> str | None:
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        left = _static_string(node.left)
        right = _static_string(node.right)
        if left is not None and right is not None:
            return left + right
    return None


def _dependency_violations(package_root: Path) -> list[str]:
    violations: list[str] = []
    for root, forbidden in FORBIDDEN.items():
        base = package_root / root
        if not base.exists():
            continue
        for source in base.rglob("*.py"):
            for imported in _imports(source, package_root):
                if imported == "<dynamic import target>":
                    violations.append(f"{source.relative_to(package_root)} -> {imported}")
                    continue
                if imported == "<dynamic code execution>":
                    violations.append(f"{source.relative_to(package_root)} -> {imported}")
                    continue
                parts = imported.split(".")
                if len(parts) >= 2 and parts[0] == "harness" and parts[1] in forbidden:
                    violations.append(f"{source.relative_to(package_root)} -> {imported}")

    for root, forbidden in FORBIDDEN_EXTERNAL.items():
        base = package_root / root
        if not base.exists():
            continue
        for source in base.rglob("*.py"):
            for imported in _imports(source, package_root):
                module = imported.split(".", maxsplit=1)[0]
                if module in forbidden:
                    violations.append(f"{source.relative_to(package_root)} -> {imported}")
    return violations


def test_internal_module_dependency_direction() -> None:
    violations = _dependency_violations(PACKAGE_ROOT)
    assert not violations, "invalid module dependencies:\n" + "\n".join(violations)


def test_architecture_guard_rejects_infrastructure_imports_from_domain(tmp_path: Path) -> None:
    package_root = tmp_path / "harness"
    world = package_root / "world"
    runtime = package_root / "runtime"
    world.mkdir(parents=True)
    runtime.mkdir()
    (world / "unsafe.py").write_text("import fastapi\n", encoding="utf-8")
    (runtime / "unsafe.py").write_text("import docker\nimport subprocess\n", encoding="utf-8")

    violations = _dependency_violations(package_root)

    assert "world/unsafe.py -> fastapi" in violations
    assert "runtime/unsafe.py -> docker" in violations
    assert "runtime/unsafe.py -> subprocess" in violations


def test_architecture_guard_rejects_static_and_unresolved_dynamic_imports(
    tmp_path: Path,
) -> None:
    package_root = tmp_path / "harness"
    world = package_root / "world"
    world.mkdir(parents=True)
    (world / "unsafe.py").write_text(
        "import importlib as loader\n"
        "import builtins as runtime_builtins\n"
        "from builtins import __import__ as load_module\n"
        "saved_import = loader.import_module\n"
        "dynamic_import = getattr(loader, 'import_module')\n"
        "dynamic_builtin_import = getattr(runtime_builtins, '__import__')\n"
        "def load(module_name):\n"
        "    loader.import_module('harness.api.runs')\n"
        "    saved_import('harness.' + 'api')\n"
        "    dynamic_import('harness.api.dynamic')\n"
        "    dynamic_builtin_import('harness.worker.extra')\n"
        "    getattr(loader, 'import_module')('harness.runtime')\n"
        "    __import__('importlib').import_module('harness.worker')\n"
        "    exec('import harness.orchestrator')\n"
        "    runtime_builtins.exec('import harness.api')\n"
        "    getattr(runtime_builtins, 'eval')('1')\n"
        "    eval(compile('import harness.cli', '<dynamic>', 'exec'))\n"
        "    load_module('harness.cli')\n"
        "    loader.import_module(module_name)\n",
        encoding="utf-8",
    )

    violations = _dependency_violations(package_root)

    assert "world/unsafe.py -> harness.api.runs" in violations
    assert "world/unsafe.py -> harness.api" in violations
    assert "world/unsafe.py -> harness.runtime" in violations
    assert "world/unsafe.py -> harness.worker" in violations
    assert "world/unsafe.py -> harness.api.dynamic" in violations
    assert "world/unsafe.py -> harness.worker.extra" in violations
    assert "world/unsafe.py -> <dynamic code execution>" in violations
    assert "world/unsafe.py -> harness.cli" in violations
    assert "world/unsafe.py -> <dynamic import target>" in violations


def test_architecture_guard_rejects_builtin_module_dynamic_execution(
    tmp_path: Path,
) -> None:
    package_root = tmp_path / "harness"
    world = package_root / "world"
    world.mkdir(parents=True)
    (world / "unsafe.py").write_text(
        "import builtins as runtime_builtins\n"
        "def load(source):\n"
        "    runtime_builtins.exec(source)\n"
        "    getattr(runtime_builtins, 'eval')('1')\n",
        encoding="utf-8",
    )

    violations = _dependency_violations(package_root)

    assert "world/unsafe.py -> <dynamic code execution>" in violations
