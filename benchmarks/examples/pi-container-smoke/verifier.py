import json
import os
from pathlib import Path

run_dir = Path(os.environ["HARNESS_RUN_DIR"])
proof = run_dir / "proof.txt"
success = proof.is_file() and proof.read_text(encoding="utf-8").strip() == "red-harness-ok"

print(
    json.dumps(
        {
            "success": success,
            "score": 100 if success else 0,
            "message": "Pi container wrote proof" if success else "proof missing or incorrect",
            "milestones": {"proof_written": success},
        }
    )
)
