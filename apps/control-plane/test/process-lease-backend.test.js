import assert from "node:assert/strict";
import test from "node:test";
import { ProcessLeaseBackend } from "../src/backend/process-lease-backend.js";

const scopeA = { consumerId: "mikan", subjectId: "subject-a" };
const scopeB = { consumerId: "mikan", subjectId: "subject-b" };

test("replays creation only within one tenant scope", async (t) => {
  const backend = new ProcessLeaseBackend();
  t.after(() => backend.close());
  const first = await backend.acquire(scopeA, { pool: "local", idempotencyKey: "request-1" });
  const replay = await backend.acquire(scopeA, { pool: "local", idempotencyKey: "request-1" });
  const other = await backend.acquire(scopeB, { pool: "local", idempotencyKey: "request-1" });
  assert.equal(replay.replayed, true);
  assert.equal(replay.lease.id, first.lease.id);
  assert.equal(other.replayed, false);
  assert.notEqual(other.lease.id, first.lease.id);
});

test("idempotency scope keys cannot collide through opaque identity contents", async (t) => {
  const backend = new ProcessLeaseBackend();
  t.after(() => backend.close());
  const first = await backend.acquire(
    { consumerId: "consumer\0subject", subjectId: "tail" },
    { pool: "local", idempotencyKey: "request" },
  );
  const second = await backend.acquire(
    { consumerId: "consumer", subjectId: "subject\0tail" },
    { pool: "local", idempotencyKey: "request" },
  );
  assert.notEqual(first.lease.id, second.lease.id);
});

test("persists files while a lease is active", async (t) => {
  const backend = new ProcessLeaseBackend();
  t.after(() => backend.close());
  const { lease } = await backend.acquire(scopeA, { pool: "local", idempotencyKey: "request-1" });
  await backend.writeFile(scopeA, lease.id, {
    path: "/workspace/value.txt",
    content: "hello",
  });
  const file = await backend.readFile(scopeA, lease.id, { path: "/workspace/value.txt" });
  assert.equal(file.content, "hello");
  assert.deepEqual(await backend.exec(scopeA, lease.id, { command: "cat value.txt" }), {
    stdout: "hello",
    stderr: "",
    code: 0,
  });
});

test("cross-scope and unknown lease lookups are indistinguishable", async (t) => {
  const backend = new ProcessLeaseBackend();
  t.after(() => backend.close());
  const { lease } = await backend.acquire(scopeA, { pool: "local", idempotencyKey: "request-1" });
  const operations = [
    () => backend.get(scopeB, lease.id),
    () => backend.get(scopeB, "lease_missing"),
    () => backend.exec(scopeB, lease.id, { command: "true" }),
    () => backend.readFile(scopeB, lease.id, { path: "/workspace/value.txt" }),
    () => backend.writeFile(scopeB, lease.id, { path: "/workspace/value.txt", content: "x" }),
    () => backend.release(scopeB, lease.id),
    () => backend.delete(scopeB, lease.id),
  ];
  for (const operation of operations) {
    await assert.rejects(async () => operation(), (error) => {
      assert.equal(error.status, 404);
      assert.equal(error.code, "LEASE_NOT_FOUND");
      assert.equal(error.message, "Lease not found");
      return true;
    });
  }
  assert.equal(backend.get(scopeA, lease.id).status, "active");
});

test("release and expiry are terminal, and paths cannot escape the workspace", async (t) => {
  let now = 1_900_000_000_000;
  const backend = new ProcessLeaseBackend({ now: () => now });
  t.after(() => backend.close());
  const { lease } = await backend.acquire(scopeA, {
    pool: "local",
    idempotencyKey: "request-1",
    ttlSeconds: 60,
  });
  await assert.rejects(
    backend.writeFile(scopeA, lease.id, { path: "/etc/passwd", content: "no" }),
    (error) => error.code === "INVALID_PATH",
  );
  const released = await backend.release(scopeA, lease.id);
  assert.equal(released.status, "released");
  const replacement = await backend.acquire(scopeA, {
    pool: "local",
    idempotencyKey: "request-1",
    ttlSeconds: 60,
  });
  assert.notEqual(replacement.lease.id, lease.id);
  await assert.rejects(
    backend.exec(scopeA, lease.id, { command: "true" }),
    (error) => error.code === "LEASE_NOT_ACTIVE",
  );

  const expiring = await backend.acquire(scopeA, {
    pool: "local",
    idempotencyKey: "request-2",
    ttlSeconds: 60,
  });
  now += 60_001;
  assert.equal(backend.get(scopeA, expiring.lease.id).status, "expired");
  await assert.rejects(
    backend.exec(scopeA, expiring.lease.id, { command: "true" }),
    (error) => error.code === "LEASE_NOT_ACTIVE",
  );
  const afterExpiry = await backend.acquire(scopeA, {
    pool: "local",
    idempotencyKey: "request-2",
    ttlSeconds: 60,
  });
  assert.notEqual(afterExpiry.lease.id, expiring.lease.id);
});

test("rejects lease durations outside platform policy", async (t) => {
  const backend = new ProcessLeaseBackend({ maxTtlSeconds: 3600 });
  t.after(() => backend.close());
  for (const ttlSeconds of [0, -1, 3601, 1.5]) {
    await assert.rejects(
      backend.acquire(scopeA, {
        pool: "local",
        idempotencyKey: `request-${ttlSeconds}`,
        ttlSeconds,
      }),
      (error) => error.code === "INVALID_LEASE_TTL",
    );
  }
});
