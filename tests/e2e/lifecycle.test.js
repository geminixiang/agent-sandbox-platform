import assert from "node:assert/strict";
import test from "node:test";
import { SandboxPlatformClient, SandboxPlatformError } from "../../packages/sdk-typescript/src/index.js";
import { ProcessSandboxBackend } from "../../apps/control-plane/src/backend/process-backend.js";
import { createControlPlaneServer } from "../../apps/control-plane/src/server.js";

test("SDK drives the control-plane lifecycle", async (t) => {
  const backend = new ProcessSandboxBackend();
  const server = createControlPlaneServer({ backend, token: "test-token" });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    await new Promise((resolve) => server.close(resolve));
    await backend.close();
  });
  const address = server.address();
  const baseUrl = `http://127.0.0.1:${address.port}`;

  const client = new SandboxPlatformClient({ baseUrl, token: "test-token" });
  const { sandbox, reused } = await client.acquire({ key: "e2e", pool: "local" });
  assert.equal(reused, false);
  await sandbox.writeFile("/workspace/message.txt", "hello platform");
  assert.equal(await sandbox.readFile("/workspace/message.txt"), "hello platform");
  assert.deepEqual(await sandbox.exec("cat message.txt"), {
    stdout: "hello platform",
    stderr: "",
    code: 0,
  });

  const second = await client.acquire({ key: "e2e", pool: "local" });
  assert.equal(second.reused, true);
  assert.equal(second.sandbox.id, sandbox.id);
  await sandbox.release();
  await assert.rejects(sandbox.exec("true"), (error) => {
    assert.ok(error instanceof SandboxPlatformError);
    assert.equal(error.code, "NOT_READY");
    return true;
  });
  await sandbox.delete();
});

test("control plane rejects invalid authentication", async (t) => {
  const backend = new ProcessSandboxBackend();
  const server = createControlPlaneServer({ backend, token: "test-token" });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    await new Promise((resolve) => server.close(resolve));
    await backend.close();
  });
  const address = server.address();
  const client = new SandboxPlatformClient({ baseUrl: `http://127.0.0.1:${address.port}` });
  await assert.rejects(client.acquire({ key: "e2e", pool: "local" }), (error) => {
    assert.equal(error.status, 401);
    assert.equal(error.code, "UNAUTHORIZED");
    return true;
  });
});
