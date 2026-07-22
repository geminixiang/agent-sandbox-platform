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
const child = spawn(binary, [], {
  cwd: new URL("../..", import.meta.url),
  env: childEnv(port),
  stdio: ["ignore", "ignore", "pipe"],
});
let stderr = "";
child.stderr.setEncoding("utf8").on("data", (chunk) => {
  stderr += chunk;
});

let lease;
try {
  await waitForReady(baseUrl, child, () => stderr);
  const client = new SandboxPlatformClient({
    baseUrl,
    consumerId: "e2e",
    subjectId: "subject-a",
    consumerSecret,
  });
  ({ lease } = await client.acquire(
    { pool: "coding", ttlSeconds: 120 },
    { idempotencyKey: `e2e-${Date.now()}` },
  ));
  await lease.writeFile("/workspace/message.txt", "hello from Go on Kubernetes");
  assert.equal(await lease.readFile("/workspace/message.txt"), "hello from Go on Kubernetes");
  const result = await lease.exec("uname -r; id -u; id -g; cat message.txt", { cwd: "/workspace" });
  assert.equal(result.code, 0);
  const [kernel, uid, gid, ...file] = result.stdout.split("\n");
  assert.match(kernel, /gvisor/);
  assert.equal(uid, "10001");
  assert.equal(gid, "10001");
  assert.equal(file.join("\n"), "hello from Go on Kubernetes");

  child.kill("SIGTERM");
  await onceExit(child);
  const restarted = spawn(binary, [], {
    cwd: new URL("../..", import.meta.url),
    env: childEnv(port),
    stdio: ["ignore", "ignore", "pipe"],
  });
  stderr = "";
  restarted.stderr.setEncoding("utf8").on("data", (chunk) => { stderr += chunk; });
  await waitForReady(baseUrl, restarted, () => stderr);
  assert.equal((await client.get(lease.id)).id, lease.id);
  await lease.release();
  lease = undefined;
  restarted.kill("SIGTERM");
  await onceExit(restarted);
  console.log("Go Kubernetes Agent Sandbox E2E passed");
} finally {
  if (child.exitCode === null) {
    child.kill("SIGTERM");
    await onceExit(child);
  }
  if (lease) await deleteClaim(lease.id).catch(() => undefined);
  await rm(buildDir, { recursive: true, force: true });
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
