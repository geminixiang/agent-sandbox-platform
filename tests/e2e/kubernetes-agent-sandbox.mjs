import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:net";
import { promisify } from "node:util";
import { SandboxPlatformClient } from "../../packages/sdk-typescript/src/index.js";

const execFileAsync = promisify(execFile);
const context = process.env.SANDBOX_E2E_KUBECONTEXT;
if (!context) throw new Error("SANDBOX_E2E_KUBECONTEXT is required");
const namespace = process.env.SANDBOX_E2E_NAMESPACE ?? "agent-sandbox-platform-e2e";
const consumerSecret = "e2e-consumer-secret";
const metadataSecret = "e2e-metadata-secret";

const port = await availablePort();
const baseUrl = `http://127.0.0.1:${port}`;
const buildDir = await mkdtemp(join(tmpdir(), "sandbox-control-plane-e2e-"));
const binary = join(buildDir, "control-plane");
await execFileAsync("go", ["build", "-o", binary, "./apps/control-plane-go/cmd/control-plane"], {
  cwd: new URL("../..", import.meta.url),
});
let processHandle = startControlPlane();
let stderr = "";
watchStderr(processHandle);
const cleanupLeaseIds = new Set();
try {
  await waitForReady(baseUrl, processHandle, () => stderr);
  const subjectA = clientFor("subject-a");
  const subjectB = clientFor("subject-b");
  const suffix = Date.now();
  const leaseA1 = await acquire(subjectA, `e2e-a1-${suffix}`);
  const leaseA2 = await acquire(subjectA, `e2e-a2-${suffix}`);
  const leaseB = await acquire(subjectB, `e2e-b-${suffix}`);

  await leaseA1.writeFile("/workspace/message.txt", "hello from Go on Kubernetes");
  assert.equal(await leaseA1.readFile("/workspace/message.txt"), "hello from Go on Kubernetes");

  const first = await subjectA.listPage({ pool: "coding", limit: 1 });
  assert.equal(first.leases.length, 1);
  assert.ok(first.nextCursor);
  assert.ok([leaseA1.id, leaseA2.id].includes(first.leases[0].id));
  const subjectAIds = new Set();
  for await (const lease of subjectA.list({ pool: "coding", limit: 1 })) subjectAIds.add(lease.id);
  assert.deepEqual(subjectAIds, new Set([leaseA1.id, leaseA2.id]));
  const subjectBIds = new Set();
  for await (const lease of subjectB.list({ pool: "coding", limit: 1 })) subjectBIds.add(lease.id);
  assert.deepEqual(subjectBIds, new Set([leaseB.id]));
  await assert.rejects(
    subjectB.listPage({ pool: "coding", limit: 1, cursor: first.nextCursor }),
    (error) => error.code === "INVALID_CURSOR",
  );

  processHandle.kill("SIGTERM");
  await onceExit(processHandle);
  processHandle = startControlPlane();
  stderr = "";
  watchStderr(processHandle);
  await waitForReady(baseUrl, processHandle, () => stderr);

  const continuedIds = new Set([first.leases[0].id]);
  let continuation = first.nextCursor;
  while (continuation !== null) {
    const continued = await subjectA.listPage({ pool: "coding", limit: 1, cursor: continuation });
    for (const lease of continued.leases) {
      assert.equal(continuedIds.has(lease.id), false, `duplicate lease ${lease.id}`);
      continuedIds.add(lease.id);
    }
    continuation = continued.nextCursor;
  }
  assert.deepEqual(continuedIds, new Set([leaseA1.id, leaseA2.id]));

  const connected = await subjectA.connect(leaseA1.id);
  const result = await connected.exec("uname -r; id -u; id -g; cat message.txt", { cwd: "/workspace" });
  assert.equal(result.code, 0);
  const [kernel, uid, gid, ...file] = result.stdout.split("\n");
  assert.match(kernel, /gvisor/);
  assert.equal(uid, "10001");
  assert.equal(gid, "10001");
  assert.equal(file.join("\n"), "hello from Go on Kubernetes");

  for (const lease of [leaseA1, leaseA2, leaseB]) await lease.release();
  await waitForClaimsDeleted([leaseA1.id, leaseA2.id, leaseB.id]);
  cleanupLeaseIds.clear();
  console.log("Go Kubernetes Agent Sandbox discovery E2E passed");
} finally {
  if (processHandle.exitCode === null) {
    processHandle.kill("SIGTERM");
    await onceExit(processHandle);
  }
  for (const id of cleanupLeaseIds) await deleteClaim(id).catch(() => undefined);
  await rm(buildDir, { recursive: true, force: true });
}

function startControlPlane() {
  return spawn(binary, [], {
    cwd: new URL("../..", import.meta.url),
    env: childEnv(port),
    stdio: ["ignore", "ignore", "pipe"],
  });
}
function watchStderr(handle) {
  handle.stderr.setEncoding("utf8").on("data", (chunk) => { stderr += chunk; });
}
function clientFor(subjectId) {
  return new SandboxPlatformClient({ baseUrl, consumerId: "e2e", subjectId, consumerSecret });
}
async function acquire(client, idempotencyKey) {
  const { lease } = await client.acquire(
    { pool: "coding", ttlSeconds: 120 },
    { idempotencyKey },
  );
  cleanupLeaseIds.add(lease.id);
  return lease;
}

function childEnv(targetPort) {
  return {
    ...process.env,
    SANDBOX_ADDRESS: `127.0.0.1:${targetPort}`,
    SANDBOX_K8S_CONTEXT: context,
    SANDBOX_K8S_NAMESPACE: namespace,
    SANDBOX_METADATA_SECRET: metadataSecret,
    SANDBOX_CONSUMER_SECRETS: JSON.stringify({ e2e: consumerSecret }),
    SANDBOX_K8S_POOLS: JSON.stringify({ coding: { warmPoolName: "platform-gvisor", runtimeClassName: "gvisor", containerName: "shell" } }),
    SANDBOX_DEFAULT_TTL_SECONDS: "120",
    SANDBOX_MAX_TTL_SECONDS: "600",
    SANDBOX_K8S_READY_TIMEOUT: "2m",
    SANDBOX_SWEEP_INTERVAL: "1s",
  };
}

async function availablePort() {
  const server = createServer();
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  const { port: value } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return value;
}
async function waitForReady(url, processHandle, logs) {
  for (let attempt = 0; attempt < 400; attempt += 1) {
    if (processHandle.exitCode !== null) throw new Error(`Go control plane exited: ${logs()}`);
    try { if ((await fetch(`${url}/ready`)).ok) return; } catch {}
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`Go control plane not ready: ${logs()}`);
}
async function onceExit(processHandle) { if (processHandle.exitCode === null) await new Promise((resolve) => processHandle.once("exit", resolve)); }
async function deleteClaim(id) { await execFileAsync("kubectl", ["--context", context, "-n", namespace, "delete", "sandboxclaim", id, "--ignore-not-found=true", "--wait=true"]); }
async function waitForClaimsDeleted(ids) {
  for (let attempt = 0; attempt < 300; attempt += 1) {
    const results = await Promise.all(ids.map(async (id) => {
      try {
        await execFileAsync("kubectl", ["--context", context, "-n", namespace, "get", "sandboxclaim", id]);
        return false;
      } catch {
        return true;
      }
    }));
    if (results.every(Boolean)) return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`released SandboxClaims were not deleted: ${ids.join(", ")}`);
}
