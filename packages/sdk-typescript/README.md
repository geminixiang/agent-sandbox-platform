# Agent Sandbox TypeScript SDK

Zero-runtime-dependency ESM client for Node.js 22.19 or newer.

```js
import { createHash } from "node:crypto";
import { SandboxPlatformClient } from "@geminixiang/sandbox-sdk";

const client = new SandboxPlatformClient({
  baseUrl: "https://sandbox.example.com",
  consumerId: "consumer",
  subjectId: "subject",
  consumerSecret: process.env.SANDBOX_CONSUMER_SECRET,
});

const { lease } = await client.acquire({ pool: "coding" });
const content = new TextEncoder().encode("hello");
const sha256 = createHash("sha256").update(content).digest("hex");

async function* chunks() {
  yield content;
}

await lease.writeFileStream("/workspace/input.bin", chunks(), {
  sizeBytes: content.byteLength,
  sha256,
});

const download = await lease.readFileStream("/workspace/output.bin");
try {
  console.log(download.sizeBytes, download.sha256);
  for await (const chunk of download) {
    consume(chunk);
  }
} finally {
  await download.close();
}
```

SHA-256 values at the SDK interface are lowercase 64-character hexadecimal strings. Downloads validate exact length and digest at normal EOF. Breaking iteration calls the iterator's `return()` and closes the response without integrity validation. Uploads use raw async iterables and Node fetch's `duplex: "half"`; neither direction eagerly buffers the full transfer.

Discover active Leases with either a page or an async iterable. The SDK continues across valid empty intermediate pages and rejects a repeated cursor rather than looping forever:

```js
const page = await client.listPage({ pool: "coding", limit: 50 });
for (const lease of page.leases) console.log(lease.id);

for await (const lease of client.list({ pool: "coding", limit: 50 })) {
  const connected = await client.connect(lease.id);
  console.log((await connected.exec("pwd")).stdout);
}
```

`nextCursor` is opaque and is `null` on the final page. Discovery order is not recency order, and `record.lastUsedAt` is not recency-safe. `SandboxInvalidCursorError`, `SandboxCursorExpiredError`, and `SandboxUnknownPoolError` expose the corresponding protocol errors. A listed Lease can be released concurrently, so `connect` may still return `LEASE_NOT_FOUND`; `get` remains available as an equivalent lookup.

The existing `readFile` and `writeFile` convenience methods remain on the legacy JSON endpoints. The production Kubernetes backend implements streaming; optional backends without this capability may return `501 STREAMING_NOT_SUPPORTED`. Saturated production transfer limits return `429 TRANSFER_LIMIT_REACHED` through `SandboxPlatformError`.
