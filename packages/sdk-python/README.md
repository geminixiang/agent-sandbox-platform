# Agent Sandbox Python SDK

Async-first Python interface for the Agent Sandbox Platform.

```python
from agent_sandbox import SandboxClient, StaticToken

async with SandboxClient(
    base_url="https://sandbox.example.com",
    credentials=StaticToken("short-lived-subject-token"),
) as client:
    async with client.sandbox(pool="coding") as sandbox:
        await sandbox.files.write_text("/workspace/main.py", "print('hello')")
        result = await sandbox.run("python main.py")
        print(result.stdout)
```

With `check=True`, a non-zero exit raises `CommandFailedError` without discarding diagnostics:

```python
from agent_sandbox import CommandFailedError

try:
    await sandbox.run("python main.py", check=True)
except CommandFailedError as error:
    print(error.command)
    print(error.result.exit_code)
    print(error.result.stdout)
    print(error.result.stderr)
```

The SDK does not expose Kubernetes, Pods, CRDs, namespaces, or runtime classes.
