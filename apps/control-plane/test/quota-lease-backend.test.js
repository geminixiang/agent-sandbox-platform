import assert from "node:assert/strict";
import test from "node:test";
import { QuotaLeaseBackend } from "../src/backend/quota-lease-backend.js";

class FakeBackend {
  constructor() {
    this.active = [];
    this.byKey = new Map();
    this.created = 0;
  }
  scopeHash(scope) {
    return `${scope.consumerId}:${scope.subjectId}`;
  }
  consumerHash(scope) {
    return scope.consumerId;
  }
  findByIdempotencyKey(scope, key) {
    return this.byKey.get(`${this.scopeHash(scope)}:${key}`);
  }
  listActiveLeases() {
    return this.active;
  }
  async acquire(scope, request) {
    await new Promise((resolve) => setTimeout(resolve, 5));
    const record = {
      id: `lease_${++this.created}`,
      pool: request.pool,
      status: "active",
      createdAt: "now",
      expiresAt: "later",
      lastUsedAt: "now",
    };
    this.active.push({
      record,
      scopeHash: this.scopeHash(scope),
      consumerHash: this.consumerHash(scope),
    });
    this.byKey.set(`${this.scopeHash(scope)}:${request.idempotencyKey}`, record);
    return { lease: record, replayed: false };
  }
  close() {}
}

const scopeA = { consumerId: "mikan", subjectId: "a" };
const scopeB = { consumerId: "mikan", subjectId: "b" };
const scopeC = { consumerId: "quro", subjectId: "a" };

function request(key, pool = "coding") {
  return { pool, idempotencyKey: key };
}

test("serializes concurrent acquisition and enforces tenant quota atomically", async () => {
  const delegate = new FakeBackend();
  const backend = new QuotaLeaseBackend(delegate, {
    perScope: 1,
    perConsumer: 10,
    perPool: 10,
  });
  const results = await Promise.allSettled([
    backend.acquire(scopeA, request("one")),
    backend.acquire(scopeA, request("two")),
  ]);
  assert.equal(results.filter((result) => result.status === "fulfilled").length, 1);
  const rejected = results.find((result) => result.status === "rejected");
  assert.equal(rejected.reason.code, "LEASE_QUOTA_EXCEEDED");
  assert.equal(delegate.created, 1);
});

test("idempotency replay bypasses quota without creating a second lease", async () => {
  const delegate = new FakeBackend();
  const backend = new QuotaLeaseBackend(delegate, { perScope: 1 });
  const first = await backend.acquire(scopeA, request("same"));
  const replay = await backend.acquire(scopeA, request("same"));
  assert.equal(replay.replayed, true);
  assert.equal(replay.lease.id, first.lease.id);
  assert.equal(delegate.created, 1);
});

test("enforces independent consumer and pool ceilings", async () => {
  const consumerBackend = new QuotaLeaseBackend(new FakeBackend(), {
    perScope: 10,
    perConsumer: 1,
    perPool: 10,
  });
  await consumerBackend.acquire(scopeA, request("one"));
  await assert.rejects(
    consumerBackend.acquire(scopeB, request("two")),
    (error) => error.code === "LEASE_QUOTA_EXCEEDED",
  );

  const poolBackend = new QuotaLeaseBackend(new FakeBackend(), {
    perScope: 10,
    perConsumer: 10,
    perPool: 1,
  });
  await poolBackend.acquire(scopeA, request("one"));
  await assert.rejects(
    poolBackend.acquire(scopeC, request("two")),
    (error) => error.code === "LEASE_QUOTA_EXCEEDED",
  );
  const otherPool = await poolBackend.acquire(scopeC, request("three", "browser"));
  assert.equal(otherPool.lease.pool, "browser");
});
