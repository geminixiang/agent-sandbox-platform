import { pathToFileURL } from "node:url";
import { KubernetesLeaseBackend } from "./backend/kubernetes-lease-backend.js";
import { ProcessLeaseBackend } from "./backend/process-lease-backend.js";
import { QuotaLeaseBackend } from "./backend/quota-lease-backend.js";
import { createControlPlaneServer } from "./server.js";

export async function startControlPlane(options = {}) {
  const backend = options.backend ?? createBackendFromEnvironment();
  const resolveConsumerSecret = options.resolveConsumerSecret ?? loadConsumerSecrets();
  await backend.recover?.();

  const server = createControlPlaneServer({ backend, resolveConsumerSecret });
  const host = options.host ?? process.env.SANDBOX_HOST ?? "127.0.0.1";
  const port = Number(options.port ?? process.env.SANDBOX_PORT ?? 8787);
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host, resolve);
  });

  const sweepIntervalMs = numberEnv("SANDBOX_SWEEP_INTERVAL_MS", 30_000);
  const sweepTimer = backend.sweepExpired
    ? setInterval(() => backend.sweepExpired().catch(reportSweepError), sweepIntervalMs)
    : undefined;
  sweepTimer?.unref();

  const close = async () => {
    if (sweepTimer) clearInterval(sweepTimer);
    await new Promise((resolve) => server.close(resolve));
    await backend.close();
  };
  return { server, backend, close };
}

function createBackendFromEnvironment() {
  const backendName = process.env.SANDBOX_BACKEND ?? "process";
  const metadataSecret =
    backendName === "kubernetes"
      ? requiredEnv("SANDBOX_METADATA_SECRET")
      : process.env.SANDBOX_METADATA_SECRET ?? "process-development-only";
  const common = {
    metadataSecret,
    defaultTtlSeconds: numberEnv("SANDBOX_DEFAULT_TTL_SECONDS", 900),
    maxTtlSeconds: numberEnv("SANDBOX_MAX_TTL_SECONDS", 3600),
  };
  const delegate =
    backendName === "process"
      ? new ProcessLeaseBackend(common)
      : backendName === "kubernetes"
        ? new KubernetesLeaseBackend({
            ...common,
            namespace: requiredEnv("SANDBOX_K8S_NAMESPACE"),
            kubeContext: process.env.SANDBOX_K8S_CONTEXT,
            pools: jsonObjectEnv("SANDBOX_K8S_POOLS"),
            readyTimeoutMs: numberEnv("SANDBOX_K8S_READY_TIMEOUT_MS", 120_000),
            pollIntervalMs: numberEnv("SANDBOX_K8S_POLL_INTERVAL_MS", 500),
          })
        : undefined;
  if (!delegate) throw new Error(`Unsupported SANDBOX_BACKEND '${backendName}'`);

  return new QuotaLeaseBackend(delegate, {
    perScope: numberEnv("SANDBOX_QUOTA_PER_SCOPE", 2),
    perConsumer: numberEnv("SANDBOX_QUOTA_PER_CONSUMER", 20),
    perPool: numberEnv("SANDBOX_QUOTA_PER_POOL", 50),
  });
}

function loadConsumerSecrets() {
  const secrets = jsonObjectEnv("SANDBOX_CONSUMER_SECRETS");
  return (consumerId) =>
    Object.hasOwn(secrets, consumerId) && typeof secrets[consumerId] === "string"
      ? secrets[consumerId]
      : undefined;
}

function jsonObjectEnv(name) {
  const raw = requiredEnv(name);
  let value;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error(`${name} must be a JSON object`);
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${name} must be a JSON object`);
  }
  return value;
}

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function numberEnv(name, fallback) {
  const raw = process.env[name];
  const value = raw === undefined ? fallback : Number(raw);
  if (!Number.isInteger(value) || value <= 0) throw new Error(`${name} must be a positive integer`);
  return value;
}

function reportSweepError(error) {
  console.error("Lease expiry sweep failed", error);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const { server, close } = await startControlPlane();
  const address = server.address();
  console.log(`Sandbox control plane listening on http://${address.address}:${address.port}`);
  process.once("SIGINT", close);
  process.once("SIGTERM", close);
}
