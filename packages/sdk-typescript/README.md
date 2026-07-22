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

The existing `readFile` and `writeFile` convenience methods remain on the legacy JSON endpoints. The production Kubernetes backend implements streaming; optional backends without this capability may return `501 STREAMING_NOT_SUPPORTED`. Saturated production transfer limits return `429 TRANSFER_LIMIT_REACHED` through `SandboxPlatformError`.
