import assert from "node:assert/strict";
import test from "node:test";
import {
  CommandFailedError,
  SandboxAbortedError,
  SandboxClient,
  SandboxError,
  SandboxFileNotFoundError,
  SandboxIntegrityError,
  SandboxNotActiveError,
  SandboxNotFoundError,
  SandboxQuotaExceededError,
  SandboxStreamingNotSupportedError,
  SandboxTransferLimitError,
  SandboxTransferTooLargeError,
  StaticToken,
} from "../src/index.js";

const RECORD = {
  id: "lease_1",
  pool: "coding",
  status: "active",
  createdAt: "2026-01-01T00:00:00.000Z",
  expiresAt: "2026-01-01T00:15:00.000Z",
  lastUsedAt: "2026-01-01T00:00:00.000Z",
};

function json(body, status = 200) {
  return new Response(body === undefined ? undefined : JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function client(fetch, credentials = new StaticToken("subject-token")) {
  return new SandboxClient({ baseUrl: "https://sandbox.example", credentials, fetch });
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

test("rotating sync and async credentials are resolved for every operation", async () => {
  let calls = 0;
  const tokens = [];
  const sdk = client(async (_url, init) => {
    tokens.push(init.headers.authorization);
    return json({ lease: RECORD });
  }, async () => `token-${++calls}`);

  await sdk.get("lease_1");
  await sdk.get("lease_1");
  assert.equal(calls, 2);
  assert.deepEqual(tokens, ["Bearer token-1", "Bearer token-2"]);

  const staticToken = new StaticToken("fixed");
  assert.equal(staticToken.getToken(), "fixed");
  assert.throws(() => new StaticToken("  "), /non-empty/);
});

test("credential resolution passes abort context, honors cancellation, and redacts failures", async () => {
  let providerSignal;
  const timedOut = client(async () => json({ lease: RECORD }), ({ signal }) => {
    providerSignal = signal;
    return new Promise((_resolve, reject) => {
      signal.addEventListener("abort", () => reject(new Error("provider stopped with a secret")), { once: true });
    });
  });
  await assert.rejects(timedOut.get("lease_1", { timeoutMs: 5 }), SandboxAbortedError);
  assert.ok(providerSignal instanceof AbortSignal);
  assert.equal(providerSignal.aborted, true);

  let objectContext;
  const objectProvider = {
    getToken(context) {
      objectContext = context;
      return "object-token";
    },
  };
  await client(async () => json({ lease: RECORD }), objectProvider).get("lease_1");
  assert.ok(objectContext.signal instanceof AbortSignal);

  let calls = 0;
  const controller = new AbortController();
  controller.abort(new Error("stop"));
  const preAborted = client(async () => json({ lease: RECORD }), () => {
    calls += 1;
    return "token";
  });
  await assert.rejects(preAborted.get("lease_1", { signal: controller.signal }), SandboxAbortedError);
  assert.equal(calls, 0);

  const empty = client(async () => json({ lease: RECORD }), async () => " ");
  await assert.rejects(empty.get("lease_1"), (error) =>
    error instanceof SandboxError &&
      error.code === "CREDENTIALS_FAILED" &&
      error.message === "Sandbox credentials could not be resolved" &&
      error.cause === undefined,
  );

  const secret = "never-appear-in-errors";
  const failed = client(async () => json({ lease: RECORD }), () => {
    throw new SandboxError(secret, { cause: new Error(secret) });
  });
  await assert.rejects(failed.get("lease_1"), (error) => {
    assert.ok(error instanceof SandboxError);
    assert.equal(error.code, "CREDENTIALS_FAILED");
    assert.equal(error.message, "Sandbox credentials could not be resolved");
    assert.equal(error.cause, undefined);
    return true;
  });
});

test("subjectToken only invokes generated legacy credentials or StaticToken", () => {
  const staticClient = client(async () => json({ lease: RECORD }), new StaticToken("fixed"));
  assert.equal(staticClient.subjectToken(), "fixed");

  let calls = 0;
  const dynamicClient = client(async () => json({ lease: RECORD }), () => {
    calls += 1;
    return "dynamic";
  });
  assert.throws(() => dynamicClient.subjectToken(), /unavailable for dynamic credentials/);
  assert.equal(calls, 0);
});

test("create facade, callback lifecycle, async disposal, and repeated release are exact-once", async () => {
  let releases = 0;
  let creates = 0;
  const sdk = client(async (url, init) => {
    const path = new URL(url).pathname;
    if (path === "/v1/leases") {
      creates += 1;
      return json({ lease: RECORD, replayed: false }, 201);
    }
    if (path.endsWith("/release")) {
      releases += 1;
      return json({ lease: { ...RECORD, status: "released" } });
    }
    throw new Error(`unexpected ${init.method} ${path}`);
  });

  const value = await sdk.sandbox({ pool: "coding" }, async (sandbox) => {
    assert.equal(sandbox.id, "lease_1");
    return 42;
  });
  assert.equal(value, 42);
  assert.equal(creates, 1);
  assert.equal(releases, 1);

  const sandbox = await sdk.create({ pool: "coding" });
  const first = await sandbox.release();
  const second = await sandbox.release();
  await sandbox.close();
  await sandbox[Symbol.asyncDispose]();
  assert.equal(first, second);
  assert.equal(releases, 2);

  const disposed = await sdk.create({ pool: "coding" });
  await disposed[Symbol.asyncDispose]();
  await disposed[Symbol.asyncDispose]();
  assert.equal(releases, 3);

  await sdk[Symbol.asyncDispose]();
  await sdk.close();
  await assert.rejects(sdk.get("lease_1"), (error) => error.code === "CLIENT_CLOSED");
});

test("sandbox callback preserves callback and cleanup failures deterministically", async () => {
  const callbackFailure = new Error("callback failed");
  const cleanupFailure = new SandboxError("cleanup failed");
  let cleanupShouldFail = false;
  const sdk = client(async (url) => {
    const path = new URL(url).pathname;
    if (path === "/v1/leases") return json({ lease: RECORD, replayed: false }, 201);
    if (path.endsWith("/release")) {
      if (cleanupShouldFail) throw cleanupFailure;
      return json({ lease: { ...RECORD, status: "released" } });
    }
    throw new Error(`unexpected ${path}`);
  });

  await assert.rejects(
    sdk.sandbox({ pool: "coding" }, async () => { throw callbackFailure; }),
    (error) => error === callbackFailure,
  );

  cleanupShouldFail = true;
  await assert.rejects(
    sdk.sandbox({ pool: "coding" }, async () => 42),
    (error) => error === cleanupFailure,
  );
  await assert.rejects(
    sdk.sandbox({ pool: "coding" }, async () => { throw callbackFailure; }),
    (error) => {
      assert.ok(error instanceof AggregateError);
      assert.deepEqual(error.errors, [callbackFailure, cleanupFailure]);
      return true;
    },
  );
});

test("terminal release, delete, close, and disposal share one first-wins transition", async () => {
  const releaseGate = deferred();
  const deleteGate = deferred();
  const requests = [];
  const sdk = client(async (url, init) => {
    const path = new URL(url).pathname;
    requests.push(`${init.method ?? "GET"} ${path}`);
    if (path.endsWith("/release")) return releaseGate.promise;
    if (init.method === "DELETE") return deleteGate.promise;
    return json({ lease: { ...RECORD, id: path.split("/").at(-1) } });
  });

  const releaseFirst = await sdk.get("release-first");
  const release = releaseFirst.release();
  const joiningRelease = releaseFirst.release();
  const losingDelete = releaseFirst.delete();
  const joiningClose = releaseFirst.close();
  const joiningDispose = releaseFirst[Symbol.asyncDispose]();
  await new Promise(setImmediate);
  assert.equal(
    requests.filter((request) => request === "POST /v1/leases/release-first/release").length,
    1,
  );
  releaseGate.resolve(json({ lease: { ...RECORD, id: "release-first", status: "released" } }));
  const releasedRecord = await release;
  assert.equal(await joiningRelease, releasedRecord);
  assert.equal(releasedRecord.status, "released");
  assert.equal(await losingDelete, undefined);
  assert.equal(await joiningClose, undefined);
  assert.equal(await joiningDispose, undefined);
  assert.equal(await releaseFirst.release(), releasedRecord);
  await releaseFirst.delete();
  assert.equal(
    requests.filter((request) => request === "POST /v1/leases/release-first/release").length,
    1,
  );

  const deleteFirst = await sdk.get("delete-first");
  const deletion = deleteFirst.delete();
  const joiningDelete = deleteFirst.delete();
  const losingRelease = deleteFirst.release();
  const deleteClose = deleteFirst.close();
  await new Promise(setImmediate);
  assert.equal(requests.filter((request) => request === "DELETE /v1/leases/delete-first").length, 1);
  deleteGate.resolve(new Response(null, { status: 204 }));
  await deletion;
  assert.equal(await joiningDelete, undefined);
  assert.equal(await losingRelease, deleteFirst.record);
  await deleteClose;
  await deleteFirst.release();
  await deleteFirst.delete();
  assert.equal(requests.filter((request) => request === "DELETE /v1/leases/delete-first").length, 1);
});

test("run returns immutable diagnostics and check raises CommandFailedError", async () => {
  const sdk = client(async (url) => {
    if (new URL(url).pathname.endsWith("/exec")) {
      return json({ stdout: "partial\n", stderr: "trace\n", code: 17 });
    }
    return json({ lease: RECORD });
  });
  const sandbox = await sdk.get("lease_1");
  const result = await sandbox.run("broken");
  assert.equal(result.exitCode, 17);
  assert.equal(result.code, 17);
  assert.equal(result.succeeded, false);
  assert.equal(Object.isFrozen(result), true);
  assert.throws(() => { result.stdout = "changed"; }, TypeError);
  await assert.rejects(sandbox.run("broken", { check: true }), (error) => {
    assert.ok(error instanceof CommandFailedError);
    assert.equal(error.command, "broken");
    assert.equal(error.result, error.result);
    assert.equal(error.result.stderr, "trace\n");
    return true;
  });
});

test("records, pages, and arrays are defensive immutable snapshots with legacy aliases", async () => {
  const wireRecord = { ...RECORD };
  const sdk = client(async (url) => {
    if (new URL(url).pathname === "/v1/leases") {
      return json({ leases: [wireRecord], nextCursor: null });
    }
    return json({ lease: wireRecord });
  });
  const page = await sdk.listPage();
  wireRecord.pool = "mutated";
  assert.equal(page.sandboxes[0].record.pool, "coding");
  assert.equal(page.leases, page.sandboxes);
  assert.equal(Object.isFrozen(page), true);
  assert.equal(Object.isFrozen(page.sandboxes), true);
  assert.equal(Object.isFrozen(page.sandboxes[0].record), true);
  assert.throws(() => page.sandboxes.push(page.sandboxes[0]), TypeError);

  const invalidStatus = client(async () => json({ lease: { ...RECORD, status: "future" } }));
  await assert.rejects(invalidStatus.get("lease_1"), /invalid lease status/);
});

test("text and bytes facade uses canonical base64 and legacy methods still delegate", async () => {
  const writes = [];
  let invalid = false;
  const sdk = client(async (url, init) => {
    const path = new URL(url).pathname;
    if (path.endsWith("/files/write")) {
      writes.push(JSON.parse(init.body));
      return json({ path: "/workspace/a" });
    }
    if (path.endsWith("/files/read")) {
      const request = JSON.parse(init.body);
      if (request.encoding === "base64") {
        return json({ content: invalid ? "Zg" : "AP9m", encoding: "base64" });
      }
      return json({ content: "hello", encoding: "utf8" });
    }
    return json({ lease: RECORD });
  });
  const sandbox = await sdk.get("lease_1");
  assert.equal(await sandbox.files.readText("/workspace/a"), "hello");
  assert.deepEqual(await sandbox.files.readBytes("/workspace/a"), new Uint8Array([0, 255, 102]));
  await sandbox.files.writeText("/workspace/a", "hello");
  await sandbox.files.writeBytes("/workspace/a", new Uint8Array([0, 255, 102]));
  await sandbox.writeFile("/workspace/a", "aGVsbG8=", { encoding: "base64" });
  assert.deepEqual(writes.map(({ encoding, content }) => [encoding, content]), [
    ["utf8", "hello"],
    ["base64", "AP9m"],
    ["base64", "aGVsbG8="],
  ]);
  invalid = true;
  await assert.rejects(sandbox.files.readBytes("/workspace/a"), (error) => {
    assert.ok(error instanceof SandboxIntegrityError);
    assert.equal(error.code, "INVALID_FILE_CONTENT");
    return true;
  });
});

test("all stable protocol errors map to public subclasses and unknown codes stay base", async () => {
  const cases = [
    ["LEASE_NOT_FOUND", SandboxNotFoundError],
    ["LEASE_NOT_ACTIVE", SandboxNotActiveError],
    ["LEASE_QUOTA_EXCEEDED", SandboxQuotaExceededError],
    ["ABORTED", SandboxAbortedError],
    ["FILE_NOT_FOUND", SandboxFileNotFoundError],
    ["TRANSFER_TOO_LARGE", SandboxTransferTooLargeError],
    ["TRANSFER_LIMIT_REACHED", SandboxTransferLimitError],
    ["CONTENT_LENGTH_MISMATCH", SandboxIntegrityError],
    ["CONTENT_DIGEST_MISMATCH", SandboxIntegrityError],
    ["INVALID_CONTENT_DIGEST", SandboxIntegrityError],
    ["STREAMING_NOT_SUPPORTED", SandboxStreamingNotSupportedError],
  ];
  for (const [code, ErrorType] of cases) {
    const sdk = client(async () => json({ error: { code, message: "safe" } }, 400));
    await assert.rejects(sdk.get("lease_1"), (error) => {
      assert.ok(error instanceof ErrorType, code);
      assert.equal(error.code, code);
      assert.equal(error.status, 400);
      return true;
    });
  }
  const sdk = client(async () => json({ error: { code: "FUTURE_CODE", message: "future" } }, 418));
  await assert.rejects(sdk.get("lease_1"), (error) => {
    assert.equal(error.constructor, SandboxError);
    assert.equal(error.cause, undefined);
    return true;
  });
});

test("caller abort and local timeout normalize to SandboxAbortedError", async () => {
  const waiting = client(() => new Promise(() => {}));
  await assert.rejects(waiting.get("lease_1", { timeoutMs: 5 }), SandboxAbortedError);

  const controller = new AbortController();
  const operation = waiting.get("lease_1", { signal: controller.signal });
  controller.abort(new Error("cancelled"));
  await assert.rejects(operation, SandboxAbortedError);
});
