import json
import os
from pathlib import Path

answer = Path(os.environ["HARNESS_RUN_DIR"]) / "answer.txt"
try:
    success = answer.read_text(encoding="utf-8") == "flag{crypto_fixture}\n"
except OSError:
    success = False
print(json.dumps({"success": success, "score": 100 if success else 0,
                  "message": "plaintext verified" if success else "plaintext invalid",
                  "milestones": {"cipher_decrypted": success}}))
