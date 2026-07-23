import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  CommandFailedError,
  SandboxAbortedError,
  SandboxClient,
  SandboxNotFoundError,
  SandboxUnknownPoolError,
} from "@geminixiang/sandbox-sdk";

const CHUNK_BYTES = 64 * 1024;
const STREAM_BYTES = 10 * 1024 * 1024;
const PAYLOAD_BLOCK = createHash("sha256").update("agent-sandbox-typescript-sdk-e2e").digest();

function required(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function payloadChunk(size) {
  const chunk = Buffer.allocUnsafe(size);
  for (let offset = 0; offset < size; offset += PAYLOAD_BLOCK.length) {
    PAYLOAD_BLOCK.copy(chunk, offset, 0, Math.min(PAYLOAD_BLOCK.length, size - offset));
  }
  return chunk;
}

function expectedStreamDigest(size) {
  const digest = createHash("sha256");
  for (let remaining = size; remaining > 0; remaining -= CHUNK_BYTES) {
    digest.update(payloadChunk(Math.min(CHUNK_BYTES, remaining)));
  }
  return digest.digest("hex");
}

function streamChunks(size, integrity) {
  return (async function* () {
    for (let remaining = size; remaining > 0; remaining -= CHUNK_BYTES) {
      const chunk = payloadChunk(Math.min(CHUNK_BYTES, remaining));
      assert.ok(chunk.byteLength <= CHUNK_BYTES);
      integrity.bytes += chunk.byteLength;
      integrity.digest.update(chunk);
      yield chunk;
    }
  })();
}

function rotatingProvider(tokens) {
  const state = { calls: 0, slots: new Set() };
  return {
    state,
    credentials: async ({ signal } = {}) => {
      assert.ok(signal instanceof AbortSignal);
      await Promise.resolve();
      if (signal.aborted) throw signal.reason;
      const slot = state.calls % tokens.length;
      state.calls += 1;
      state.slots.add(slot);
      return tokens[slot];
    },
  };
}

async function assertNotVisible(client, id) {
  await assert.rejects(client.connect(id), (error) => {
    assert.ok(error instanceof SandboxNotFoundError);
    assert.equal(error.code, "LEASE_NOT_FOUND");
    return true;
  });
}

async function verifyStreaming(sandbox, pool) {
  const path = `/workspace/typescript-${pool}-stream.bin`;
  const expected = expectedStreamDigest(STREAM_BYTES);
  const upload = { bytes: 0, digest: createHash("sha256") };
  await sandbox.files.writeStream(path, streamChunks(STREAM_BYTES, upload), {
    sizeBytes: STREAM_BYTES,
    sha256: expected,
    timeoutMs: 180_000,
  });
  assert.equal(upload.bytes, STREAM_BYTES);
  assert.equal(upload.digest.digest("hex"), expected);

  const download = await sandbox.files.readStream(path, { timeoutMs: 180_000 });
  assert.equal(download.sizeBytes, STREAM_BYTES);
  assert.equal(download.sha256, expected);
  const downloaded = createHash("sha256");
  let downloadedBytes = 0;
  for await (const transportChunk of download) {
    for (let offset = 0; offset < transportChunk.byteLength; offset += CHUNK_BYTES) {
      const chunk = transportChunk.subarray(offset, Math.min(offset + CHUNK_BYTES, transportChunk.byteLength));
      assert.ok(chunk.byteLength <= CHUNK_BYTES);
      downloaded.update(chunk);
      downloadedBytes += chunk.byteLength;
    }
  }
  await download.close();
  assert.equal(downloadedBytes, STREAM_BYTES);
  assert.equal(downloaded.digest("hex"), expected);
  return expected;
}

async function verifyBrowser(sandbox) {
  const source = `import { chromium } from "playwright-core";
const browser = await chromium.launch({ executablePath: "/usr/bin/chromium", headless: true });
try {
  const page = await browser.newPage();
  await page.setContent("<main><h1>TypeScript SDK acceptance</h1></main>");
  console.log(JSON.stringify({ heading: await page.locator("h1").textContent() }));
} finally {
  await browser.close();
}
`;
  await sandbox.files.writeText("/workspace/typescript-browser.mjs", source);
  const result = await sandbox.run(
    "test -e node_modules || ln -s /opt/browser/node_modules node_modules; node typescript-browser.mjs",
    { cwd: "/workspace", check: true, timeoutSeconds: 60, timeoutMs: 90_000 },
  );
  assert.deepEqual(JSON.parse(result.stdout), { heading: "TypeScript SDK acceptance" });
}

async function verifyPool(client, pool) {
  const sandbox = await client.create({
    pool,
    ttlSeconds: 300,
    idempotencyKey: `typescript-${pool}-${Date.now()}-${process.hrtime.bigint()}`,
  });
  let released = false;
  try {
    assert.ok(Object.isFrozen(sandbox.record));
    assert.throws(() => {
      sandbox.record.status = "released";
    }, TypeError);

    const textPath = `/workspace/typescript-${pool}.txt`;
    await sandbox.files.writeText(textPath, `persistent-${pool}`);
    assert.equal(await sandbox.files.readText(textPath), `persistent-${pool}`);

    const canonicalBytes = new Uint8Array([0x00, 0xff, 0x66, 0x80, 0x7f, 0x01]);
    const bytesPath = `/workspace/typescript-${pool}.bin`;
    await sandbox.files.writeBytes(bytesPath, canonicalBytes);
    const readBytes = await sandbox.files.readBytes(bytesPath);
    assert.ok(readBytes instanceof Uint8Array);
    assert.deepEqual(readBytes, canonicalBytes);

    const success = await sandbox.run("printf 'sdk-out'; printf 'sdk-err' >&2", { check: true });
    assert.equal(success.stdout, "sdk-out");
    assert.equal(success.stderr, "sdk-err");
    assert.equal(success.exitCode, 0);
    assert.equal(success.succeeded, true);
    assert.ok(Object.isFrozen(success));
    assert.throws(() => {
      success.exitCode = 9;
    }, TypeError);

    await assert.rejects(
      sandbox.run("printf 'kept-out'; printf 'kept-err' >&2; exit 23", { check: true }),
      (error) => {
        assert.ok(error instanceof CommandFailedError);
        assert.equal(error.command, "printf 'kept-out'; printf 'kept-err' >&2; exit 23");
        assert.equal(error.result.stdout, "kept-out");
        assert.equal(error.result.stderr, "kept-err");
        assert.equal(error.result.exitCode, 23);
        assert.equal(error.result.succeeded, false);
        assert.ok(Object.isFrozen(error.result));
        return true;
      },
    );

    const page = await client.listPage({ pool, limit: 1 });
    assert.ok(Object.isFrozen(page));
    assert.ok(Object.isFrozen(page.sandboxes));
    assert.ok(page.sandboxes.some(({ id }) => id === sandbox.id));
    assert.throws(() => {
      page.nextCursor = "changed";
    }, TypeError);

    const listed = [];
    for await (const item of client.list({ pool, limit: 1 })) listed.push(item.id);
    assert.ok(listed.includes(sandbox.id));

    const connected = await client.connect(sandbox.id);
    assert.equal(connected.id, sandbox.id);
    assert.ok(Object.isFrozen(connected.record));
    assert.equal(await connected.files.readText(textPath), `persistent-${pool}`);

    const sha256 = await verifyStreaming(connected, pool);
    if (pool === "browser") await verifyBrowser(connected);

    const releasedRecord = await sandbox.release();
    released = true;
    assert.equal(releasedRecord.status, "released");
    assert.ok(Object.isFrozen(releasedRecord));
    await assertNotVisible(client, sandbox.id);
    return { pool, bytes: STREAM_BYTES, chunkBytes: CHUNK_BYTES, sha256 };
  } finally {
    if (!released) await sandbox.close();
  }
}

async function verifyTimeout(baseUrl) {
  let providerObservedAbort = false;
  const client = new SandboxClient({
    baseUrl,
    timeoutMs: 1_000,
    credentials: ({ signal } = {}) => new Promise((resolve, reject) => {
      signal.addEventListener("abort", () => {
        providerObservedAbort = true;
        reject(signal.reason);
      }, { once: true });
    }),
  });
  try {
    await assert.rejects(client.listPage({ timeoutMs: 25 }), SandboxAbortedError);
    assert.equal(providerObservedAbort, true);
  } finally {
    await client.close();
  }
}

async function main() {
  const startedAt = Date.now();
  const baseUrl = required("SANDBOX_PLATFORM_URL");
  const ownerTokens = [required("SANDBOX_TEST_OWNER_TOKEN_A"), required("SANDBOX_TEST_OWNER_TOKEN_B")];
  const otherToken = required("SANDBOX_TEST_OTHER_TOKEN");
  const rotating = rotatingProvider(ownerTokens);
  const otherState = { calls: 0 };
  const owner = new SandboxClient({ baseUrl, credentials: rotating.credentials, timeoutMs: 180_000 });
  const other = new SandboxClient({
    baseUrl,
    credentials: async () => {
      otherState.calls += 1;
      await Promise.resolve();
      return otherToken;
    },
    timeoutMs: 180_000,
  });

  try {
    await verifyTimeout(baseUrl);

    await assert.rejects(
      owner.create({ pool: "typescript-unknown-pool", ttlSeconds: 300 }),
      (error) => {
        assert.ok(error instanceof SandboxUnknownPoolError);
        assert.equal(error.code, "UNKNOWN_POOL");
        return true;
      },
    );

    const callbackID = await owner.sandbox(
      { pool: "coding", ttlSeconds: 300, idempotencyKey: `typescript-callback-${Date.now()}` },
      async (sandbox) => {
        await sandbox.files.writeText("/workspace/callback.txt", "callback-cleanup");
        assert.equal(await sandbox.files.readText("/workspace/callback.txt"), "callback-cleanup");
        return sandbox.id;
      },
    );
    await assertNotVisible(owner, callbackID);

    const isolationOwner = await owner.create({
      pool: "coding",
      ttlSeconds: 300,
      idempotencyKey: `typescript-isolation-owner-${Date.now()}`,
    });
    let ownerReleased = false;
    try {
      await isolationOwner.files.writeText("/workspace/owner-only.txt", "owner");
      const otherPage = await other.listPage({ pool: "coding", limit: 100 });
      assert.ok(!otherPage.sandboxes.some(({ id }) => id === isolationOwner.id));
      await assertNotVisible(other, isolationOwner.id);

      const isolationOther = await other.create({
        pool: "coding",
        ttlSeconds: 300,
        idempotencyKey: `typescript-isolation-other-${Date.now()}`,
      });
      let otherReleased = false;
      try {
        await isolationOther.files.writeText("/workspace/other-only.txt", "other");
        const ownerPage = await owner.listPage({ pool: "coding", limit: 100 });
        assert.ok(!ownerPage.sandboxes.some(({ id }) => id === isolationOther.id));
        await assertNotVisible(owner, isolationOther.id);
        const otherReleasedRecord = await isolationOther.release();
        otherReleased = true;
        assert.equal(otherReleasedRecord.status, "released");
      } finally {
        if (!otherReleased) await isolationOther.close();
      }
      await assertNotVisible(other, isolationOther.id);
      const ownerReleasedRecord = await isolationOwner.release();
      ownerReleased = true;
      assert.equal(ownerReleasedRecord.status, "released");
    } finally {
      if (!ownerReleased) await isolationOwner.close();
    }
    await assertNotVisible(owner, isolationOwner.id);

    const pools = [];
    for (const pool of ["coding", "browser"]) pools.push(await verifyPool(owner, pool));

    assert.ok(rotating.state.calls >= 20, `token provider was called only ${rotating.state.calls} times`);
    assert.deepEqual([...rotating.state.slots].sort(), [0, 1]);
    assert.ok(otherState.calls >= 5, `second Subject token was called only ${otherState.calls} times`);

    process.stdout.write(`${JSON.stringify({
      status: "passed",
      coverage: {
        durationMs: Date.now() - startedAt,
        rotatingAsyncTokenProvider: { calls: rotating.state.calls, tokenSlots: rotating.state.slots.size },
        callbackCleanup: true,
        commandFailureRetention: true,
        discoveryAndPersistence: true,
        immutableSnapshots: true,
        subjectIsolation: true,
        timeoutAbort: true,
        typedUnknownPool: true,
        textAndCanonicalBytes: true,
        browserLaunch: true,
        streaming: pools,
      },
    })}\n`);
  } finally {
    await Promise.allSettled([owner.close(), other.close()]);
  }
}

await main();
