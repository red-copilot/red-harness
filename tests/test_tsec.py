import json

from redharness.benchmark.tsec import extract_flags


def test_extract_flags_from_pi_jsonl() -> None:
    events = [
        {
            "type": "message_end",
            "message": {
                "role": "assistant",
                "content": [{"type": "text", "text": "REDHARNESS_FLAG=flag{one}"}],
            },
        },
        {
            "type": "message_end",
            "message": {
                "role": "assistant",
                "content": "REDHARNESS_FLAG=flag{two} REDHARNESS_FLAG=flag{one}",
            },
        },
    ]
    raw = "\n".join(json.dumps(item) for item in events)
    assert extract_flags(raw) == ["flag{one}", "flag{two}"]
