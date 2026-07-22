import assert from "node:assert/strict";
import test from "node:test";
import {
  LeaseHandle,
  SandboxPlatformClient,
  SandboxPlatformError,
} from "../../packages/sdk-typescript/src/index.js";
import { ProcessLeaseBackend } from "../../apps/control-plane/src/backend/process-lease-backend.js";
import { createControlPlaneServer } from "../../apps/control-plane/src/server.js";

const consumers = { mikan: "mikan-secret", quro: "quro-secret" };

async function startTestServer(t) {
  const backend = new ProcessLeaseBackend();
  const server = createControlPlaneServer({
    backend,
    resolveConsumerSecret: (consumerId) => consumers[consumerId],
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    await new Promise((resolve) => server.close(resolve));
    await backend.close();
  });
  const address = server.address();
  return { backend, baseUrl: `http://127.0.0.1:${address.port}` };
}

function client(baseUrl, consumerId, subjectId) {
  return new SandboxPlatformClient({
    baseUrl,
    consumerId,
    subjectId,
    consumerSecret: consumers[consumerId],
  });
}

test("SDK drives the lease lifecycle", async (t) => {
  const { baseUrl } = await startTestServer(t);
  const mikan = client(baseUrl, "mikan", "subject-a");
  const { lease, replayed } = await mikan.acquire(
    { pool: "local" },
    { idempotencyKey: "request-1" },
  );
  assert.equal(replayed, false);
  await lease.writeFile("/workspace/message.txt", "hello platform");
  assert.equal(await lease.readFile("/workspace/message.txt"), "hello platform");
  assert.deepEqual(await lease.exec("cat message.txt"), {
    stdout: "hello platform",
    stderr: "",
    code: 0,
  });

  const replay = await mikan.acquire({ pool: "local" }, { idempotencyKey: "request-1" });
  assert.equal(replay.replayed, true);
  assert.equal(replay.lease.id, lease.id);
  await lease.release();
  await assert.rejects(lease.exec("true"), (error) => {
    assert.equal(error.code, "LEASE_NOT_ACTIVE");
    return true;
  });
  await lease.delete();
});

test("every lease operation blocks cross-subject access without disclosure", async (t) => {
  const { baseUrl } = await startTestServer(t);
  const owner = client(baseUrl, "mikan", "subject-a");
  const attacker = client(baseUrl, "mikan", "subject-b");
  const { lease } = await owner.acquire({ pool: "local" }, { idempotencyKey: "request-1" });
  await lease.writeFile("/workspace/secret.txt", "subject-a-secret");

  const attackerHandle = new LeaseHandle(attacker, lease.record);
  const operations = [
    () => attacker.get(lease.id),
    () => attackerHandle.exec("cat secret.txt"),
    () => attackerHandle.readFile("/workspace/secret.txt"),
    () => attackerHandle.writeFile("/workspace/secret.txt", "stolen"),
    () => attackerHandle.release(),
    () => attackerHandle.delete(),
  ];
  for (const operation of operations) await expectLeaseNotFound(operation);
  await expectLeaseNotFound(() => attacker.get("lease_does_not_exist"));

  assert.equal(await lease.readFile("/workspace/secret.txt"), "subject-a-secret");
  assert.equal((await lease.refresh()).status, "active");
});

test("same subject and idempotency key remain isolated across consumers", async (t) => {
  const { baseUrl } = await startTestServer(t);
  const mikan = client(baseUrl, "mikan", "shared-subject");
  const quro = client(baseUrl, "quro", "shared-subject");
  const first = await mikan.acquire({ pool: "local" }, { idempotencyKey: "same-request" });
  const second = await quro.acquire({ pool: "local" }, { idempotencyKey: "same-request" });
  assert.notEqual(first.lease.id, second.lease.id);
  await expectLeaseNotFound(() => quro.get(first.lease.id));
});

test("request bodies cannot override the authenticated tenant scope", async (t) => {
  const { baseUrl } = await startTestServer(t);
  const owner = client(baseUrl, "mikan", "subject-a");
  const response = await owner.request("/v1/leases", {
    method: "POST",
    headers: { "idempotency-key": "forged-scope" },
    body: {
      pool: "local",
      consumerId: "quro",
      subjectId: "subject-b",
    },
  });
  assert.equal("consumerId" in response.lease, false);
  assert.equal("subjectId" in response.lease, false);

  const attacker = client(baseUrl, "quro", "subject-b");
  await expectLeaseNotFound(() => attacker.get(response.lease.id));
  assert.equal((await owner.get(response.lease.id)).id, response.lease.id);
});

test("control plane rejects missing authentication", async (t) => {
  const { baseUrl } = await startTestServer(t);
  const response = await fetch(`${baseUrl}/v1/leases`, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": "request-1" },
    body: JSON.stringify({ pool: "local" }),
  });
  assert.equal(response.status, 401);
  assert.deepEqual(await response.json(), {
    error: { code: "UNAUTHORIZED", message: "Invalid or expired subject token" },
  });
});

async function expectLeaseNotFound(operation) {
  await assert.rejects(operation, (error) => {
    assert.ok(error instanceof SandboxPlatformError);
    assert.equal(error.status, 404);
    assert.equal(error.code, "LEASE_NOT_FOUND");
    assert.equal(error.message, "Lease not found");
    return true;
  });
}
