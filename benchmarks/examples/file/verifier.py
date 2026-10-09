import json
import os
from pathlib import Path

receipt = Path(os.environ["HARNESS_RUN_DIR"]) / "receipt.txt"
try:
    success = receipt.read_bytes() == b"harness-fixture-accepted\n"
except OSError:
    success = False

print(json.dumps({"success": success, "score": 100 if success else 0,
                  "message": "receipt verified" if success else "receipt invalid",
                  "milestones": {"receipt_exact": success}}))
