import json
import os
from pathlib import Path

answer = Path(os.environ["HARNESS_RUN_DIR"]) / "answer.txt"
try:
    success = answer.read_text(encoding="utf-8") == "flag{reverse_fixture}\n"
except OSError:
    success = False
print(json.dumps({"success": success, "score": 100 if success else 0,
                  "message": "decoded string verified" if success else "decoded string invalid",
                  "milestones": {"string_decoded": success}}))
