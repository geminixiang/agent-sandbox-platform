# Python SDK parity review: Agent Sandbox Platform vs E2B

Date: 2026-07-22  
E2B source reviewed at commit [`e5a4bd655d29439dae67c269e806db1cad74d7ba`](https://github.com/e2b-dev/E2B/tree/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk).  
Platform source reviewed at commit `29de7e2`.

## Executive conclusion

The SDKs overlap on the minimum agent workload loop—create a sandbox, execute a foreground command, move small files, inspect exit output, and clean up—but they do **not** currently offer equivalent support or interchangeable interfaces.

The platform SDK is an intentionally small, async-first client for operator-defined Kubernetes Pools. E2B's Python SDK is a much broader end-user compute SDK with sync and async APIs, process streaming and reconnection, a full filesystem API, PTY, exposed ports, lifecycle controls, snapshots/forking, templates, volumes, metrics, network controls, Git helpers, and pagination/listing.

A fair product claim today is:

> Existing Python agents whose sandbox dependency is limited to foreground command execution and small text/binary file transfer can be adapted to this platform with a thin integration layer. Existing E2B integrations generally cannot switch by changing a base URL or import; feature-rich integrations require missing platform capabilities and an adapter or application rewrite.

## Interface comparison

The closest equivalent basic flow is:

```python
# E2B
from e2b import AsyncSandbox

async with await AsyncSandbox.create(template="my-template") as sandbox:
    await sandbox.files.write("/home/user/main.py", "print('hello')")
    result = await sandbox.commands.run("python /home/user/main.py")
```

```python
# Agent Sandbox Platform
from agent_sandbox import SandboxClient, StaticToken

async with SandboxClient(base_url=url, credentials=StaticToken(token)) as client:
    async with client.sandbox(pool="coding") as sandbox:
        await sandbox.files.write_text("/workspace/main.py", "print('hello')")
        result = await sandbox.run("python /workspace/main.py", check=True)
```

Important semantic differences:

- E2B users choose a **template**; platform users choose an operator-governed logical **Pool**. A Pool hides Kubernetes image, runtime, scheduling, network policy, and warm capacity.
- E2B exposes `sandbox.commands.run`; the platform exposes `sandbox.run` directly.
- E2B supports global API-key configuration and class methods. The platform requires an explicit `SandboxClient`, control-plane URL, and token provider.
- Both support context-manager cleanup, but E2B's context exit kills the sandbox while the platform releases its Lease back through the control plane.
- The APIs are conceptually similar but neither source-compatible nor wire-compatible.

Sources: platform [`_client.py`](../../packages/sdk-python/src/agent_sandbox/_client.py), E2B [`sandbox_async/main.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/main.py), E2B [`commands/command.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/commands/command.py).

## Capability matrix

Legend: **Yes** = practical parity for the stated capability; **Partial** = basic form exists but important E2B semantics are absent; **No** = no public SDK capability.

| Capability | Platform Python SDK | E2B Python SDK | Assessment |
| --- | --- | --- | --- |
| Async API | `SandboxClient`, `Sandbox` | `AsyncSandbox` | **Yes**, different shape |
| Sync API | No | `Sandbox` mirrors async surface | **No** |
| Context-manager cleanup | Async context manager releases Lease | Sync and async context managers kill sandbox | **Yes**, lifecycle semantics differ |
| Create isolated environment | `create(pool, ttl, idempotency_key)` | `create(template, timeout, metadata, envs, secure, network, lifecycle, volumes, mcp, ...)` | **Partial** |
| Idempotent create | Explicit/generated idempotency key | No equivalent visible in public create signature | **Platform advantage** |
| Reconnect/get by ID | `client.get(id)` reconstructs a handle | `connect(id)` supports running/paused sandboxes | **Partial**; platform has no resume |
| List/filter/paginate sandboxes | No | `list()` with state/metadata query and paginator | **No** |
| Health/running status | Lease `refresh()` exposes active/released/expired | `is_running()`, `get_info()` | **Partial** |
| Update lifetime | Fixed TTL at creation | `set_timeout()` after creation | **No** |
| Kill/delete | `delete()` | instance/class `kill()` | **Yes** at basic level |
| Release/reuse capacity | Explicit `release()` into platform lifecycle | Kill-oriented user API | **Platform-specific advantage** |
| Pause/resume | No | `pause()` and `connect()` resume | **No** |
| Fork full state | No | `fork(count=...)` | **No** |
| Snapshots | No | create/list/delete snapshots | **No** |
| Foreground command | `run(command, cwd, env, timeout, check)` | `commands.run(cmd, envs, user, cwd, timeout, callbacks, stdin)` | **Partial** |
| Structured command result | immutable stdout/stderr/exit code | stdout/stderr/exit code/error | **Yes** for buffered completion |
| Typed non-zero exit | `CommandFailedError` preserves command/result | `CommandExitException` carries result | **Yes** |
| Streaming stdout/stderr | No; fully buffered | callbacks and streaming command events | **No** |
| Background commands | No | `background=True` returns command handle | **No** |
| Process list/kill/reconnect | No | list, kill, connect by PID | **No** |
| Interactive stdin | No | send/close stdin via command handle | **No** |
| PTY | No | create/connect/resize/send/kill PTY | **No** |
| Run as selected user | No | command and filesystem `user` argument | **No** |
| Text/binary file read/write | Four convenience methods | `read(format=...)`, `write(str/bytes/IO)` | **Partial** |
| Streaming file transfer | No; whole-file JSON/base64 | streamed upload/read, optional gzip | **No** |
| Multi-file write | No | `write_files()` | **No** |
| List/stat/exists | No | list, get_info, exists | **No** |
| mkdir/remove/rename | Only possible through shell commands | dedicated APIs | **No** |
| Filesystem watch | No | recursive watch handle/events | **No** |
| File metadata | No | upload metadata and entry information | **No** |
| Expose sandbox port/URL | No | `get_host(port)` for external HTTP/WebSocket access | **No** |
| Per-sandbox egress controls | Operator-controlled Pool policy only | create/update allow/deny/rules and public traffic | **No at SDK level**; intentionally operator-governed |
| Create-time environment | No | `envs` on create | **No** |
| Per-command environment | Yes | Yes | **Yes** |
| Secret reference abstraction | Token provider authenticates SDK; pi extension resolves host env references | API/access tokens and env injection | **Not equivalent** |
| Metadata/tags | No public metadata | create metadata; list filtering | **No** |
| Metrics | No | CPU/memory/disk metrics | **No** |
| Templates/build API | Pools are preconfigured by operators; no SDK build API | sync/async template build and lifecycle API | **No**, intentional responsibility split |
| Persistent volumes | Workspace persistence within current Lease/backend behavior; no volume resource API | create/connect/list/destroy volumes and mount at create | **No** |
| Git helper API | Use CLI through `run()` | clone/init/status/branch/commit/push/pull/config helpers | **No** |
| MCP bootstrap/token | No | create-time MCP config and MCP URL/token | **No** |
| Browser-specific API | Browser Pool runs standard Playwright through commands | Base E2B SDK also primarily supplies compute; separate images/packages can add browser tooling | **Comparable only at workload level**, not SDK parity |
| Typed platform errors | not found, inactive, expired, quota, aborted, command failed | auth, timeout, argument, disk, file/sandbox not found, rate, template, build, volume, command exit | **Partial** |
| Custom credential refresh | sync or async `TokenProvider` per request | API key/access token and connection options | **Yes**, different auth model |
| Custom HTTP transport/testing | Async `httpx` transport injection | connection configuration and generated transports | **Partial** |
| Package typing | `py.typed`, strict pyright in project | `py.typed`, typed public models | **Yes** |

E2B sources: public exports in [`e2b/__init__.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/__init__.py); lifecycle in [`sandbox_async/main.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/main.py); command API in [`commands/command.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/commands/command.py) and [`command_handle.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/commands/command_handle.py); filesystem in [`filesystem.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/filesystem/filesystem.py); PTY in [`commands/pty.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox_async/commands/pty.py); networking and models in [`sandbox/sandbox_api.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/sandbox/sandbox_api.py); errors in [`exceptions.py`](https://github.com/e2b-dev/E2B/blob/e5a4bd655d29439dae67c269e806db1cad74d7ba/packages/python-sdk/e2b/exceptions.py).

Platform sources: [`_client.py`](../../packages/sdk-python/src/agent_sandbox/_client.py), [`_models.py`](../../packages/sdk-python/src/agent_sandbox/_models.py), [`_errors.py`](../../packages/sdk-python/src/agent_sandbox/_errors.py), [`_credentials.py`](../../packages/sdk-python/src/agent_sandbox/_credentials.py), and [`README.md`](../../packages/sdk-python/README.md).

## What can migrate today?

### Low-friction: thin adapter is sufficient

An E2B-using agent is a realistic early migration candidate when it only needs:

1. create one environment from a known workload class;
2. run foreground shell commands;
3. collect complete stdout/stderr/exit code;
4. read/write small files;
5. apply a command timeout;
6. destroy/release the environment.

The adapter maps `template` to an approved Pool and `sandbox.commands.run()` to `sandbox.run()`. It must still adapt authentication and cleanup semantics.

### Requires platform work

Migration is not currently viable without material changes when the product depends on:

- live token streaming or long-running background processes;
- stdin, process reconnection, or PTY terminals;
- opening web servers and obtaining public/authenticated port URLs;
- snapshots, pause/resume, or fork;
- large/streamed files or filesystem watching;
- SDK-selected templates, volumes, metadata queries, or network rules;
- sync-only Python applications;
- E2B Git, MCP, template build, or volume helpers.

## Recommended parity target

Do **not** clone E2B's complete API indiscriminately. The Kubernetes-native product has an intentional operator/user boundary: Pools, runtime classes, images, secure network defaults, and scheduling belong behind the control-plane seam. Exposing raw E2B-style template and network controls could weaken that design.

For the stated customer Golden Path—connect an existing agent product and send simple work without managing machines—the highest-value SDK roadmap is:

### P0: credible drop-in workload capability

1. **Command namespace and streaming handles**: introduce `sandbox.commands.run/start/connect/list/kill`, streaming stdout/stderr, stdin, cancellation, and background process handles. Keep `sandbox.run()` as a convenience alias.
2. **Streaming filesystem**: read/write streams, list/stat/exists/mkdir/remove/rename, atomic large writes, bounded limits, and typed file errors. This also resolves issue #4.
3. **Workload ingress**: an authenticated `sandbox.get_url(port)` or controlled port-forward abstraction with Pool policy deciding whether exposure is permitted.
4. **Discovery/reconnection**: list active Sandboxes by tenant/metadata, reconnect by ID, idempotent create, and explicit kill/release semantics.
5. **Metadata and create-time environment references**: tenant-safe labels and secret references, without sending secret values in model-visible arguments.
6. **Sync facade**: only if customer evidence shows meaningful sync demand; otherwise keep async as the reference UX.

### P1: interactive and operational completeness

7. PTY create/connect/resize.
8. Sandbox info and usage metrics.
9. Update TTL and typed Pool unavailable/capacity errors with retry guidance.
10. Persistent workspace/volume abstraction governed by Pool policy.

### P2: differentiated advanced lifecycle

11. Pause/resume and snapshots if Kubernetes backend semantics can make them portable.
12. Fork only after a backend can provide real state cloning; do not emulate it misleadingly.
13. Optional Git/framework helpers should live above the small core SDK rather than expanding the protocol prematurely.

## Product positioning

Today, the platform is best positioned as a **Kubernetes-native, policy-governed sandbox execution SDK**, not as an E2B Python SDK replacement.

The differentiation is meaningful:

- logical Pools rather than user-controlled runtime infrastructure;
- tenant-scoped control plane and operator-owned secure defaults;
- portable gVisor/Kubernetes backend;
- explicit idempotency and warm-capacity release semantics;
- self-hosted control over cluster, images, scheduling, and data path.

But SDK breadth is currently the largest customer-facing gap. Reaching parity for the common agent loop requires command streaming/background processes, full streamed filesystem operations, controlled port exposure, and reconnect/list semantics before claiming that existing E2B-oriented agent products can switch directly.
