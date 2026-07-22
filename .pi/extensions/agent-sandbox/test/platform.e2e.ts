import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createHmac } from "node:crypto";
import { PlatformClient, redact, resolveSecretEnvironment, type SandboxRecord } from "../client.js";

const baseUrl = process.env.SANDBOX_PLATFORM_URL;
const consumerId = process.env.SANDBOX_TEST_CONSUMER_ID;
const subjectId = process.env.SANDBOX_TEST_SUBJECT_ID;
const consumerSecret = process.env.SANDBOX_TEST_CONSUMER_SECRET;
if (!baseUrl || !consumerId || !subjectId || !consumerSecret) throw new Error("Sandbox extension E2E environment is incomplete");

const token = () => {
  const payload = Buffer.from(JSON.stringify({ consumerId, subjectId, exp: Math.floor(Date.now()/1000)+300 })).toString("base64url");
  const signed = `v1.${payload}`;
  return `${signed}.${createHmac("sha256", consumerSecret).update(signed).digest("base64url")}`;
};
const client = new PlatformClient(new URL(baseUrl), token);
let sandbox: SandboxRecord | undefined;
try {
  sandbox = await client.create("browser", 120);
  const script = await readFile(new URL("../../../../images/browser/smoke.mjs", import.meta.url), "utf8");
  await client.writeFile(sandbox.id, "/workspace/pi-browser-smoke.mjs", script, "utf8");
  const secretValue = "pi-extension-secret-canary";
  const resolved = resolveSecretEnvironment({ TEST_SECRET: "HOST_TEST_SECRET" }, { HOST_TEST_SECRET: secretValue });
  const secretResult = await client.exec(sandbox.id, "printf '%s' \"$TEST_SECRET\"", { env: resolved.values });
  assert.equal(redact(secretResult.stdout, resolved.secrets), "[REDACTED]");
  const browserResult = await client.exec(sandbox.id, "test -e /workspace/node_modules || ln -s /opt/browser/node_modules /workspace/node_modules; node /workspace/pi-browser-smoke.mjs", { cwd: "/workspace", timeoutSeconds: 60 });
  assert.equal(browserResult.code, 0, browserResult.stderr);
  assert.match(browserResult.stdout, /"output":"clicked"/);
  const screenshot = await client.readFile(sandbox.id, "/workspace/browser-smoke.png", "base64");
  assert.ok(screenshot.length > 1000);
  if (sandbox) console.log(`Pi extension platform E2E passed (${sandbox.id})`);
} finally {
  if (sandbox) {
    const id = sandbox.id;
    await client.release(id).catch(() => client.delete(id));
  }
}
