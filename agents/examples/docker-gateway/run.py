import json
import os
import urllib.request

url = os.environ["HARNESS_GATEWAY_URL"] + "/v1/tools/call"
token = os.environ["HARNESS_GATEWAY_TOKEN"]
body = json.dumps(
    {
        "name": "file.write",
        "args": {"path": "proof.txt", "content": "red-harness-ok\n"},
    }
).encode()
request = urllib.request.Request(
    url,
    data=body,
    headers={
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    },
    method="POST",
)
with urllib.request.urlopen(request, timeout=5) as response:
    assert response.status == 200
