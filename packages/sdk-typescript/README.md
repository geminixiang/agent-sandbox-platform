# Agent Sandbox TypeScript SDK

Zero-runtime-dependency ESM client for Node.js 22.19 or newer. This package is for trusted server and worker processes, not browsers.

## Recommended usage

Supply a short-lived Subject token provider. The provider is called for every HTTP operation, so token rotation does not require recreating the client. Consumer secrets belong in the trusted token issuer and must never be shipped to a browser or passed through untrusted application code.

```js
import { SandboxClient } from "@geminixiang/sandbox-sdk";

await using client = new SandboxClient({
  baseUrl: "https://sandbox.example.com",
  credentials: async () => subjectTokenCache.getOrRefresh(),
});

const value = await client.sandbox({ pool: "coding", ttlSeconds: 900 }, async (sandbox) => {
  await sandbox.files.writeText("/workspace/input.txt", "hello");
  const result = await sandbox.run("cat /workspace/input.txt", { check: true });
  return result.stdout;
});
```

`client.sandbox(options, callback)` releases the Sandbox exactly once on callback success or failure. If both callback and cleanup fail, it throws `AggregateError` with the callback error first and cleanup error second. `SandboxClient`, `Sandbox`, and `FileDownload` also support `await using`; `close()` is idempotent. TypeScript consumers using `await using` must enable a disposable-aware TypeScript/lib configuration (for example `ESNext.Disposable`); ordinary SDK types compile with an ES2022 library alone.

Use `StaticToken` only when the caller already has a suitably short-lived Subject token:

```js
import { SandboxClient, StaticToken } from "@geminixiang/sandbox-sdk";

const client = new SandboxClient({
  baseUrl: "https://sandbox.example.com",
  credentials: new StaticToken(process.env.SANDBOX_SUBJECT_TOKEN),
});
```

## Commands and files

`run()` returns a frozen `CommandResult` with `stdout`, `stderr`, `exitCode`, and `succeeded`. Passing `check: true` raises `CommandFailedError` for a non-zero exit while retaining the command and immutable result.

```js
await sandbox.files.writeBytes("/workspace/input.bin", new Uint8Array([1, 2, 3]));
const bytes = await sandbox.files.readBytes("/workspace/output.bin");

await using download = await sandbox.files.readStream("/workspace/large.bin");
for await (const chunk of download) consume(chunk);
```

Text uses UTF-8. Byte helpers use canonical base64 on the JSON wire and return `Uint8Array`. Streaming uploads require `sizeBytes` and a lowercase SHA-256 digest. Downloads validate exact length and digest at normal EOF; intentionally closing early cancels without integrity validation. Every operation accepts `signal` and `timeoutMs`. Token providers receive an optional `{ signal }` context so asynchronous credential lookup can stop on caller cancellation or timeout; existing zero-argument providers remain compatible.

## Discovery

```js
const page = await client.listPage({ pool: "coding", limit: 50 });
for (const sandbox of page.sandboxes) console.log(sandbox.id);

for await (const sandbox of client.list({ pool: "coding", limit: 50 })) {
  console.log(sandbox.id);
}
```

Pages, page arrays, Lease records, and command results are defensive frozen snapshots. `nextCursor` is opaque and `null` on the final page. Empty intermediate pages are valid. Discovery is not recency ordered, and a concurrently released Sandbox can still fail `connect()` with `SandboxNotFoundError`.

## Migration and compatibility

| 0.1 API | 0.2 facade | Status |
| --- | --- | --- |
| `SandboxPlatformClient` | `SandboxClient` | Deprecated alias retained |
| `acquire({ pool })` | `create({ pool })` or `sandbox({ pool }, callback)` | Legacy return envelope retained |
| `LeaseHandle` | `Sandbox` | Deprecated alias retained |
| `lease.exec()` / result `code` | `sandbox.run()` / `exitCode` | Legacy shape retained; `CommandResult.code` is deprecated |
| `readFile` / `writeFile` | `files.readText`, `readBytes`, `writeText`, `writeBytes` | Deprecated methods retained |
| `readFileStream` / `writeFileStream` | `files.readStream` / `writeStream` | Deprecated methods retained |
| `LeasePage.leases` | `SandboxPage.sandboxes` | Frozen deprecated alias retained |
| consumer identity and secret constructor | short-lived `credentials` provider | Deprecated server-only compatibility path retained |

All former exports and method signatures remain available in `0.2.0-rc.1`. The only intentional new runtime restrictions are validation of empty dynamic tokens, closed-client operations, canonical base64 responses, and immutable returned snapshots.

## Errors

Stable protocol codes map to public subclasses of `SandboxError`, including not-found, inactive, quota, abort, file, transfer, integrity, streaming support, and cursor errors. Server `ABORTED`, caller cancellation, and local operation timeouts all become `SandboxAbortedError`. Unknown future codes remain `SandboxError` with `status`, `code`, and optional `cause`.

## Limits and unsupported features

- JSON request bodies are limited by the platform to 1 MiB.
- Streaming file transfers are limited to 64 MiB.
- Command output is limited by the platform to 10 MiB.
- Streaming does not support ranges, resume, compression, directory operations, or per-Sandbox storage quotas.
- Optional backends may return `SandboxStreamingNotSupportedError`; production Kubernetes supports streaming.
- Transfer saturation returns `SandboxTransferLimitError`.
- The local-process backend is development-only and is not secure isolation.
- Browser use and distributing Consumer secrets are unsupported.
