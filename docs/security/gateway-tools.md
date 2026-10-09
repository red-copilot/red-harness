# Optional Gateway tool plugins

Gateway adapters are opt-in. Supplying an adapter does not authorize it: the
Gateway policy must also include its tool name in `allowed_tools`. The default
policy exposes only `file.read` and `file.write`.

```python
from harness.gateway import create_gateway_app
from harness.policy import GatewayPolicy
from harness.tool_plugins import AuthorizedHTTPToolAdapter, SandboxedProcessToolAdapter

app = create_gateway_app(
    event_file=run_dir / "events.jsonl",
    workspace=workspace,
    task_dir=agent_visible_task_dir,
    gateway_token=token,
    policy=GatewayPolicy(allowed_tools={"file.read", "file.write", "process.run", "network.request"}),
    tool_adapters=[
        SandboxedProcessToolAdapter(),
        AuthorizedHTTPToolAdapter(allowed_cidrs=["10.20.0.0/16"], allowed_ports=[80, 443]),
    ],
)
```

`SandboxedProcessToolAdapter` requires Linux `bubblewrap` and `prlimit`. It runs
an argv directly, with a private network/PID namespace, no capabilities, a
minimal root containing read-only system binaries, a read-only `/task`, a
writable `/workspace`, a cleared environment, CPU/address-space/file-size/
process/file-descriptor limits, a wall timeout, and bounded stdout/stderr. The
adapter is not a shell-safe command parser; whoever authorizes `process.run`
authorizes arbitrary argv execution inside these constraints. If the isolation
tools are unavailable, the adapter fails closed.

`AuthorizedHTTPToolAdapter` accepts only literal IP URLs inside configured
CIDRs and configured ports. It rejects DNS names, user info, URL fragments,
disallowed methods, and line breaks in headers. Environment proxies and
redirect following are disabled. Request bodies, headers, response bytes, and
time are bounded. Configure CIDRs only for destinations that the run is
authorized to contact. Responses and process output remain untrusted tool
observations; their content hashes do not prove objective effects.

For container Agents, pass the verifier-free task snapshot as `task_dir` to
the Gateway. Never mount the original task directory into a Gateway reachable
by an untrusted Agent.
