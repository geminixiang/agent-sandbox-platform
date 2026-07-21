import assert from "node:assert/strict";
import test from "node:test";
import { SandboxPlatformClient, SandboxPlatformError } from "../src/index.js";

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

test("acquires and operates a sandbox through HTTP", async () => {
  const requests = [];
  const record = {
    id: "sbx_1",
    key: "conversation-1",
    pool: "local",
    status: "ready",
    createdAt: "2026-01-01T00:00:00.000Z",
    lastUsedAt: "2026-01-01T00:00:00.000Z",
  };
  const client = new SandboxPlatformClient({
    baseUrl: "https://sandbox.example/",
    token: "secret",
    fetch: async (url, init) => {
      requests.push({ url: String(url), init });
      if (String(url).endsWith("/acquire")) return jsonResponse({ sandbox: record, reused: false });
      if (String(url).endsWith("/exec")) return jsonResponse({ stdout: "ok\n", stderr: "", code: 0 });
      if (String(url).endsWith("/files/read")) {
        return jsonResponse({ path: "/workspace/a", content: "value", encoding: "utf8" });
      }
      throw new Error(`Unexpected URL ${url}`);
    },
  });

  const { sandbox, reused } = await client.acquire({ key: "conversation-1", pool: "local" });
  assert.equal(reused, false);
  assert.equal(sandbox.id, "sbx_1");
  assert.deepEqual(await sandbox.exec("echo ok"), { stdout: "ok\n", stderr: "", code: 0 });
  assert.equal(await sandbox.readFile("/workspace/a"), "value");
  assert.equal(requests[0].init.headers.authorization, "Bearer secret");
});

test("normalizes platform errors", async () => {
  const client = new SandboxPlatformClient({
    baseUrl: "https://sandbox.example/",
    fetch: async () => jsonResponse({ error: { code: "NOT_FOUND", message: "missing" } }, 404),
  });
  await assert.rejects(client.get("missing"), (error) => {
    assert.ok(error instanceof SandboxPlatformError);
    assert.equal(error.status, 404);
    assert.equal(error.code, "NOT_FOUND");
    return true;
  });
});
