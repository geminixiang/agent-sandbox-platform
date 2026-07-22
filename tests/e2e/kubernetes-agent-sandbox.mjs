import assert from "node:assert/strict";
import { KubernetesLeaseBackend } from "../../apps/control-plane/src/backend/kubernetes-lease-backend.js";
import { QuotaLeaseBackend } from "../../apps/control-plane/src/backend/quota-lease-backend.js";
import { createControlPlaneServer } from "../../apps/control-plane/src/server.js";
import { SandboxPlatformClient } from "../../packages/sdk-typescript/src/index.js";

const context = process.env.SANDBOX_E2E_KUBECONTEXT;
if (!context) throw new Error("SANDBOX_E2E_KUBECONTEXT is required");
const namespace = process.env.SANDBOX_E2E_NAMESPACE ?? "agent-sandbox-platform-e2e";
const consumerSecret = "e2e-consumer-secret";
const backendOptions = {
  namespace,
  kubeContext: context,
  metadataSecret: "e2e-metadata-secret",
  defaultTtlSeconds: 120,
  maxTtlSeconds: 600,
  readyTimeoutMs: 120_000,
  pools: {
    coding: {
      warmPoolName: "platform-gvisor",
      runtimeClassName: "gvisor",
      containerName: "shell",
    },
  },
};

const delegate = new KubernetesLeaseBackend(backendOptions);
const stale = await delegate.recover();
for (const entry of stale) {
  await delegate.delete({ consumerId: "e2e", subjectId: "subject-a" }, entry.record.id);
}
const backend = new QuotaLeaseBackend(delegate, { perScope: 1, perConsumer: 2, perPool: 2 });
const server = createControlPlaneServer({
  backend,
  resolveConsumerSecret: (consumerId) => (consumerId === "e2e" ? consumerSecret : undefined),
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const address = server.address();
const client = new SandboxPlatformClient({
  baseUrl: `http://127.0.0.1:${address.port}`,
  consumerId: "e2e",
  subjectId: "subject-a",
  consumerSecret,
});
let lease;
try {
  ({ lease } = await client.acquire(
    { pool: "coding", ttlSeconds: 120 },
    { idempotencyKey: `e2e-${Date.now()}` },
  ));
  await lease.writeFile("/workspace/message.txt", "hello from kubernetes");
  assert.equal(await lease.readFile("/workspace/message.txt"), "hello from kubernetes");
  const result = await lease.exec("uname -r; cat message.txt", { cwd: "/workspace" });
  assert.equal(result.code, 0);
  assert.match(result.stdout, /gvisor/);
  assert.match(result.stdout, /hello from kubernetes/);

  const recovered = new KubernetesLeaseBackend(backendOptions);
  const records = await recovered.recover();
  assert.ok(records.some((entry) => entry.record.id === lease.id));
  assert.equal((await recovered.get({ consumerId: "e2e", subjectId: "subject-a" }, lease.id)).id, lease.id);

  await assert.rejects(
    client.acquire({ pool: "coding" }, { idempotencyKey: `quota-${Date.now()}` }),
    (error) => error.code === "LEASE_QUOTA_EXCEEDED",
  );
  await lease.release();
  lease = undefined;

  for (let attempt = 0; attempt < 100; attempt += 1) {
    if ((await delegate.listActiveLeases()).length === 0) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  const expiring = await client.acquire(
    { pool: "coding", ttlSeconds: 120 },
    { idempotencyKey: `expiry-${Date.now()}` },
  );
  const realNow = delegate.now;
  delegate.now = () => realNow() + 121_000;
  assert.equal(await delegate.sweepExpired(), 1);
  delegate.now = realNow;
  await assert.rejects(client.get(expiring.lease.id), (error) => error.code === "LEASE_NOT_FOUND");

  console.log("Kubernetes Agent Sandbox E2E passed");
} finally {
  if (lease) await lease.delete().catch(() => undefined);
  await new Promise((resolve) => server.close(resolve));
  await backend.close();
}
