import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";
import { KubernetesLeaseBackend } from "../src/backend/kubernetes-lease-backend.js";

const scope = { consumerId: "mikan", subjectId: "subject-a" };

function createFixture(options = {}) {
  const claims = new Map();
  const deleted = [];
  const runtimeClassName = options.runtimeClassName ?? "gvisor";
  const customObjectsApi = {
    async createNamespacedCustomObject({ body }) {
      const claim = structuredClone(body);
      claim.metadata.creationTimestamp = claim.metadata.annotations["sandbox.geminixiang.dev/created-at"];
      claim.status = { sandbox: { name: claim.metadata.name } };
      claims.set(claim.metadata.name, claim);
      return claim;
    },
    async getNamespacedCustomObject({ group, name }) {
      if (group === "agents.x-k8s.io") {
        return {
          metadata: {
            name,
            annotations: { "agents.x-k8s.io/pod-name": name },
          },
          status: {
            conditions: [{ type: "Ready", status: "True" }],
          },
        };
      }
      const claim = claims.get(name);
      if (!claim) throw Object.assign(new Error("not found"), { statusCode: 404 });
      return structuredClone(claim);
    },
    async listNamespacedCustomObject({ labelSelector }) {
      const selectors = labelSelector.split(",").map((item) => item.split("="));
      return {
        items: Array.from(claims.values()).filter((claim) =>
          selectors.every(([key, value]) => claim.metadata.labels?.[key] === value),
        ),
      };
    },
    async deleteNamespacedCustomObject({ name }) {
      deleted.push(name);
      claims.delete(name);
      return {};
    },
  };
  const coreApi = {
    async readNamespacedPod({ name }) {
      return {
        metadata: { name },
        spec: { runtimeClassName, containers: [{ name: "shell" }] },
      };
    },
  };
  const backend = new KubernetesLeaseBackend({
    namespace: "platform-test",
    metadataSecret: "metadata-secret",
    now: options.now ?? (() => 1_900_000_000_000),
    pollIntervalMs: 1,
    readyTimeoutMs: 100,
    pools: {
      coding: {
        warmPoolName: "gvisor-pool",
        runtimeClassName: "gvisor",
        containerName: "shell",
      },
    },
    clients: { customObjectsApi, coreApi, execClient: options.execClient ?? {} },
  });
  return { backend, claims, deleted };
}

function request(key = "request-1", ttlSeconds = 60) {
  return { pool: "coding", idempotencyKey: key, ttlSeconds };
}

test("creates a claim with hashed ownership and server-side pool mapping", async () => {
  const { backend, claims } = createFixture();
  const result = await backend.acquire(scope, request());
  const claim = claims.get(result.lease.id);
  assert.equal(claim.spec.warmPoolRef.name, "gvisor-pool");
  assert.equal(claim.spec.lifecycle.shutdownPolicy, "DeleteForeground");
  assert.equal(result.lease.pool, "coding");
  assert.equal(result.lease.status, "active");

  const metadata = JSON.stringify(claim.metadata);
  assert.equal(metadata.includes(scope.consumerId), false);
  assert.equal(metadata.includes(scope.subjectId), false);
  assert.equal(metadata.includes("request-1"), false);
  assert.match(claim.metadata.labels["sandbox.geminixiang.dev/scope"], /^[a-f0-9]{40}$/);
  assert.match(claim.metadata.labels["sandbox.geminixiang.dev/consumer"], /^[a-f0-9]{40}$/);
});

test("recovers active claims and replays idempotent acquisition", async () => {
  const { backend } = createFixture();
  const first = await backend.acquire(scope, request());
  const replay = await backend.findByIdempotencyKey(scope, "request-1");
  assert.equal(replay.id, first.lease.id);
  const recovered = await backend.recover();
  assert.equal(recovered.length, 1);
  assert.equal(recovered[0].record.id, first.lease.id);
});

test("cross-scope access is indistinguishable from an unknown lease", async () => {
  const { backend } = createFixture();
  const { lease } = await backend.acquire(scope, request());
  for (const id of [lease.id, "lease-unknown"]) {
    await assert.rejects(
      backend.get({ consumerId: "mikan", subjectId: "subject-b" }, id),
      (error) => {
        assert.equal(error.status, 404);
        assert.equal(error.code, "LEASE_NOT_FOUND");
        assert.equal(error.message, "Lease not found");
        return true;
      },
    );
  }
});

test("claims being deleted are neither active nor publicly visible", async () => {
  const { backend, claims } = createFixture();
  const { lease } = await backend.acquire(scope, request());
  claims.get(lease.id).metadata.deletionTimestamp = "2026-01-01T00:00:00.000Z";
  assert.deepEqual(await backend.listActiveLeases(), []);
  assert.equal(await backend.findByIdempotencyKey(scope, "request-1"), undefined);
  await assert.rejects(
    backend.get(scope, lease.id),
    (error) => error.code === "LEASE_NOT_FOUND",
  );
});

test("release and expiry delete the underlying claim", async () => {
  let now = 1_900_000_000_000;
  const { backend, claims, deleted } = createFixture({ now: () => now });
  const first = await backend.acquire(scope, request("release", 60));
  const released = await backend.release(scope, first.lease.id);
  assert.equal(released.status, "released");
  assert.equal(claims.has(first.lease.id), false);
  const replacement = await backend.acquire(scope, request("release", 60));
  assert.notEqual(replacement.lease.id, first.lease.id);
  await backend.release(scope, replacement.lease.id);

  const second = await backend.acquire(scope, request("expire", 60));
  now += 60_001;
  assert.equal((await backend.get(scope, second.lease.id)).status, "expired");
  const afterExpiry = await backend.acquire(scope, request("expire", 60));
  assert.notEqual(afterExpiry.lease.id, second.lease.id);
  await backend.release(scope, afterExpiry.lease.id);
  assert.ok(deleted.includes(first.lease.id));
  assert.ok(deleted.includes(second.lease.id));
});

test("bounds command output retained by the control plane", async () => {
  const execClient = {
    async exec(_namespace, _pod, _container, _command, stdout, _stderr, _stdin, _tty, status) {
      stdout.write(Buffer.alloc(10 * 1024 * 1024 + 1, 97));
      status({ status: "Success" });
      const socket = new EventEmitter();
      socket.close = () => socket.emit("close");
      setImmediate(() => socket.emit("close"));
      return socket;
    },
  };
  const { backend } = createFixture({ execClient });
  const { lease } = await backend.acquire(scope, request());
  await assert.rejects(
    backend.exec(scope, lease.id, { command: "large-output" }),
    (error) => error.code === "OUTPUT_TOO_LARGE",
  );
});

test("runtime mismatch rejects the lease and cleans up the claim", async () => {
  const { backend, claims } = createFixture({ runtimeClassName: "runc" });
  await assert.rejects(
    backend.acquire(scope, request()),
    (error) => error.code === "RUNTIME_CLASS_MISMATCH",
  );
  assert.equal(claims.size, 0);
});
