import assert from "node:assert/strict";
import test from "node:test";
import { ProcessSandboxBackend } from "../src/backend/process-backend.js";

test("reuses active sandboxes and persists workspace files", async (t) => {
  const backend = new ProcessSandboxBackend();
  t.after(() => backend.close());
  const first = await backend.acquire({ key: "conversation", pool: "local" });
  const second = await backend.acquire({ key: "conversation", pool: "local" });
  assert.equal(second.reused, true);
  assert.equal(second.sandbox.id, first.sandbox.id);

  await backend.writeFile(first.sandbox.id, {
    path: "/workspace/value.txt",
    content: "hello",
  });
  const file = await backend.readFile(first.sandbox.id, { path: "/workspace/value.txt" });
  assert.equal(file.content, "hello");

  const result = await backend.exec(first.sandbox.id, { command: "cat value.txt" });
  assert.deepEqual(result, { stdout: "hello", stderr: "", code: 0 });
});

test("release prevents execution and path escapes", async (t) => {
  const backend = new ProcessSandboxBackend();
  t.after(() => backend.close());
  const { sandbox } = await backend.acquire({ key: "conversation", pool: "local" });
  await assert.rejects(
    backend.writeFile(sandbox.id, { path: "/etc/passwd", content: "no" }),
    (error) => error.code === "INVALID_PATH",
  );
  await backend.release(sandbox.id);
  await assert.rejects(
    backend.exec(sandbox.id, { command: "true" }),
    (error) => error.code === "NOT_READY",
  );
});
