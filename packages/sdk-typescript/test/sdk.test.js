import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import {
  createSubjectToken,
  SandboxPlatformClient,
  SandboxPlatformError,
  SandboxPlatformIntegrityError,
} from "../src/index.js";

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const record = {
  id: "lease_1",
  pool: "local",
  status: "active",
  createdAt: "2026-01-01T00:00:00.000Z",
  expiresAt: "2026-01-01T00:15:00.000Z",
  lastUsedAt: "2026-01-01T00:00:00.000Z",
};

test("acquires and operates a lease through authenticated HTTP", async () => {
  const requests = [];
  const client = new SandboxPlatformClient({
    baseUrl: "https://sandbox.example/",
    consumerId: "mikan",
    subjectId: "subject-a",
    consumerSecret: "secret",
    fetch: async (url, init) => {
      requests.push({ url: String(url), init });
      if (String(url).endsWith("/leases")) return jsonResponse({ lease: record, replayed: false }, 201);
      if (String(url).endsWith("/exec")) return jsonResponse({ stdout: "ok\n", stderr: "", code: 0 });
      if (String(url).endsWith("/files/read")) {
        return jsonResponse({ path: "/workspace/a", content: "value", encoding: "utf8" });
      }
      throw new Error(`Unexpected URL ${url}`);
    },
  });

  const { lease, replayed, idempotencyKey } = await client.acquire(
    { pool: "local" },
    { idempotencyKey: "request-1" },
  );
  assert.equal(replayed, false);
  assert.equal(idempotencyKey, "request-1");
  assert.equal(lease.id, "lease_1");
  assert.deepEqual(await lease.exec("echo ok"), { stdout: "ok\n", stderr: "", code: 0 });
  assert.equal(await lease.readFile("/workspace/a"), "value");
  assert.match(requests[0].init.headers.authorization, /^Bearer v1\./);
  assert.equal(requests[0].init.headers["idempotency-key"], "request-1");
  assert.deepEqual(JSON.parse(requests[0].init.body), { pool: "local" });
});

test("streams file downloads lazily and verifies wire metadata", async () => {
  const chunks = [new TextEncoder().encode("first-"), new TextEncoder().encode("second")];
  const content = Buffer.concat(chunks);
  const sha256 = createHash("sha256").update(content).digest("hex");
  let pulls = 0;
  const body = new ReadableStream({
    pull(controller) {
      if (pulls < chunks.length) controller.enqueue(chunks[pulls++]);
      else controller.close();
    },
  });
  const client = clientWithFetch(async (url, init) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    assert.match(String(url), /\/files\/content\?path=%2Fworkspace%2Fdata\.bin$/);
    assert.equal(init.headers.accept, "application/octet-stream");
    return new Response(body, {
      headers: {
        "content-type": "application/octet-stream",
        "content-length": String(content.byteLength),
        "content-digest": contentDigest(sha256),
      },
    });
  });
  const lease = await client.get("lease_1");
  const download = await lease.readFileStream("/workspace/data.bin");
  assert.equal(download.sizeBytes, content.byteLength);
  assert.equal(download.sha256, sha256);
  assert.ok(pulls < chunks.length, "download must not eagerly buffer the entire body");
  const received = [];
  for await (const chunk of download) received.push(chunk);
  assert.deepEqual(Buffer.concat(received), content);
});

test("stream upload preserves chunks and required wire headers", async () => {
  const content = Buffer.from("chunk-one|chunk-two");
  const sha256 = createHash("sha256").update(content).digest("hex");
  let generated = 0;
  let lease;
  const client = clientWithFetch(async (url, init) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    assert.equal(generated, 0, "upload must not consume chunks before fetch starts reading");
    assert.equal(init.duplex, "half");
    assert.equal(init.headers["content-type"], "application/octet-stream");
    assert.equal(init.headers["content-length"], String(content.byteLength));
    assert.equal(init.headers["content-digest"], contentDigest(sha256));
    const received = [];
    for await (const chunk of init.body) received.push(chunk);
    assert.deepEqual(Buffer.concat(received), content);
    return new Response(null, { status: 204 });
  });
  lease = await client.get("lease_1");
  async function* chunks() {
    generated++;
    yield content.subarray(0, 9);
    generated++;
    yield content.subarray(9);
  }
  await lease.writeFileStream("/workspace/data.bin", chunks(), {
    sizeBytes: content.byteLength,
    sha256,
  });
  assert.equal(generated, 2);
});

test("stream upload validates declared length before success", async () => {
  const content = new Uint8Array([1]);
  const sha256 = createHash("sha256").update(content).digest("hex");
  const client = clientWithFetch(async (url, init) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    for await (const _chunk of init.body) { /* consume */ }
    return new Response(null, { status: 204 });
  });
  const lease = await client.get("lease_1");
  async function* chunks() { yield content; }
  await assert.rejects(
    lease.writeFileStream("/workspace/data.bin", chunks(), { sizeBytes: 2, sha256 }),
    (error) => error instanceof SandboxPlatformIntegrityError && error.code === "CONTENT_LENGTH_MISMATCH",
  );
});

test("download early close cancels without integrity validation", async () => {
  const sha256 = createHash("sha256").update("complete body").digest("hex");
  let canceled = false;
  const body = new ReadableStream({
    pull(controller) { controller.enqueue(new Uint8Array([1])); },
    cancel() { canceled = true; },
  });
  const client = clientWithFetch(async (url) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    return new Response(body, { headers: {
      "content-type": "application/octet-stream",
      "content-length": "13",
      "content-digest": contentDigest(sha256),
    }});
  });
  const lease = await client.get("lease_1");
  const download = await lease.readFileStream("/workspace/data.bin");
  for await (const _chunk of download) break;
  assert.equal(canceled, true);
});

test("download truncation and streaming preflight errors are typed", async () => {
  const sha256 = createHash("sha256").update("expected").digest("hex");
  const truncatedClient = clientWithFetch(async (url) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    return new Response(new Uint8Array([1, 2]), { headers: {
      "content-type": "application/octet-stream",
      "content-length": "8",
      "content-digest": contentDigest(sha256),
    }});
  });
  const truncatedLease = await truncatedClient.get("lease_1");
  const download = await truncatedLease.readFileStream("/workspace/data.bin");
  await assert.rejects(async () => {
    for await (const _chunk of download) { /* consume */ }
  }, (error) => error instanceof SandboxPlatformIntegrityError && error.code === "CONTENT_LENGTH_MISMATCH");

  const unsupportedClient = clientWithFetch(async (url) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    return jsonResponse({ error: { code: "STREAMING_NOT_SUPPORTED", message: "not supported" } }, 501);
  });
  const unsupportedLease = await unsupportedClient.get("lease_1");
  await assert.rejects(unsupportedLease.readFileStream("/workspace/data.bin"), (error) => {
    assert.ok(error instanceof SandboxPlatformError);
    assert.equal(error.status, 501);
    assert.equal(error.code, "STREAMING_NOT_SUPPORTED");
    return true;
  });

  const limitedClient = clientWithFetch(async (url) => {
    if (!String(url).includes("/files/content")) return jsonResponse({ lease: record });
    return jsonResponse({ error: { code: "TRANSFER_LIMIT_REACHED", message: "busy" } }, 429);
  });
  const limitedLease = await limitedClient.get("lease_1");
  await assert.rejects(limitedLease.readFileStream("/workspace/data.bin"), (error) => {
    assert.ok(error instanceof SandboxPlatformError);
    assert.equal(error.status, 429);
    assert.equal(error.code, "TRANSFER_LIMIT_REACHED");
    return true;
  });
});

test("stream upload maps a transport end to ABORTED", async () => {
  const client = new SandboxPlatformClient({
    baseUrl: "https://sandbox.example/",
    consumerId: "mikan",
    subjectId: "subject-a",
    consumerSecret: "secret",
    fetch: async (url) => {
      if (String(url).endsWith("/leases/lease_1")) return jsonResponse({ lease: record });
      throw new TypeError("fetch failed");
    },
  });
  const lease = await client.get("lease_1");
  await assert.rejects(
    lease.writeFileStream("/workspace/a", (async function* () { yield new Uint8Array([120]); })(), {
      sizeBytes: 1,
      sha256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881",
    }),
    (error) => error instanceof SandboxPlatformError && error.code === "ABORTED",
  );
});

test("creates verifiable short-lived subject claims without exposing the secret", () => {
  const token = createSubjectToken({
    consumerId: "mikan",
    subjectId: "opaque-subject",
    consumerSecret: "never-send-this",
    expiresAt: 2_000_000_000,
  });
  assert.match(token, /^v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  assert.equal(token.includes("never-send-this"), false);
});

function clientWithFetch(fetch) {
  return new SandboxPlatformClient({
    baseUrl: "https://sandbox.example/",
    consumerId: "mikan",
    subjectId: "subject-a",
    consumerSecret: "secret",
    fetch,
  });
}

function contentDigest(sha256) {
  return `sha-256=:${Buffer.from(sha256, "hex").toString("base64")}:`;
}

test("normalizes platform errors", async () => {
  const client = new SandboxPlatformClient({
    baseUrl: "https://sandbox.example/",
    consumerId: "mikan",
    subjectId: "subject-a",
    consumerSecret: "secret",
    fetch: async () => jsonResponse({ error: { code: "LEASE_NOT_FOUND", message: "Lease not found" } }, 404),
  });
  await assert.rejects(client.get("missing"), (error) => {
    assert.ok(error instanceof SandboxPlatformError);
    assert.equal(error.status, 404);
    assert.equal(error.code, "LEASE_NOT_FOUND");
    return true;
  });
});
