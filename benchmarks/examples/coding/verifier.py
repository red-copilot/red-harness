import ast
import json
import os
from pathlib import Path

solution = Path(os.environ["HARNESS_RUN_DIR"]) / "solution.py"
success = False
if solution.is_file():
    try:
        module = ast.parse(solution.read_text(encoding="utf-8"))
        success = (
            len(module.body) == 1
            and isinstance(module.body[0], ast.FunctionDef)
            and module.body[0].name == "solve"
            and not module.body[0].args.args
            and len(module.body[0].body) == 1
            and isinstance(module.body[0].body[0], ast.Return)
            and isinstance(module.body[0].body[0].value, ast.Constant)
            and type(module.body[0].body[0].value.value) is int
            and module.body[0].body[0].value.value == 42
        )
    except (OSError, UnicodeDecodeError, SyntaxError):
        success = False

print(json.dumps({"success": success, "score": 100 if success else 0,
                  "message": "solution verified" if success else "solution invalid",
                  "milestones": {"solve_returns_42": success}}))
