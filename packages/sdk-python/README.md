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

For bounded binary transfers, provide or consume chunks without buffering the whole file. SHA-256 values use lowercase 64-character hexadecimal strings at the SDK interface:

```python
import hashlib

content = b"..."
sha256 = hashlib.sha256(content).hexdigest()

async def chunks():
    yield content[:1024]
    yield content[1024:]

await sandbox.files.write_stream(
    "/workspace/input.bin",
    chunks(),
    size_bytes=len(content),
    sha256=sha256,
)

async with sandbox.files.read_stream("/workspace/output.bin") as download:
    print(download.size_bytes, download.sha256)
    async for chunk in download:
        consume(chunk)
```

`FileDownload` owns the streaming HTTP response. Use it as an async context manager so normal EOF validates length and digest and early exit closes the response. `SandboxIntegrityError` reports truncation or digest mismatch. `SandboxStreamingNotSupportedError` reports a backend without this optional capability. The existing text and bytes convenience methods intentionally continue to use the legacy JSON endpoints.

The current Kubernetes backend does not support these streaming methods yet and returns `501 STREAMING_NOT_SUPPORTED`; no Kubernetes streaming support is claimed in this stage.

The SDK does not expose Kubernetes, Pods, CRDs, namespaces, or runtime classes.
