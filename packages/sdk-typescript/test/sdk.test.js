import assert from "node:assert/strict";
import test from "node:test";
import { createSubjectToken, SandboxPlatformClient, SandboxPlatformError } from "../src/index.js";

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
