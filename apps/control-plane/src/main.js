import { pathToFileURL } from "node:url";
import { ProcessLeaseBackend } from "./backend/process-lease-backend.js";
import { createControlPlaneServer } from "./server.js";

export async function startControlPlane(options = {}) {
  const backend = options.backend ?? new ProcessLeaseBackend();
  const resolveConsumerSecret = options.resolveConsumerSecret ?? loadConsumerSecrets();
  const server = createControlPlaneServer({ backend, resolveConsumerSecret });
  const host = options.host ?? process.env.SANDBOX_HOST ?? "127.0.0.1";
  const port = Number(options.port ?? process.env.SANDBOX_PORT ?? 8787);
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host, resolve);
  });
  return { server, backend };
}

function loadConsumerSecrets() {
  const raw = process.env.SANDBOX_CONSUMER_SECRETS;
  if (!raw) throw new Error("SANDBOX_CONSUMER_SECRETS is required");
  let secrets;
  try {
    secrets = JSON.parse(raw);
  } catch {
    throw new Error("SANDBOX_CONSUMER_SECRETS must be a JSON object");
  }
  if (!secrets || typeof secrets !== "object" || Array.isArray(secrets)) {
    throw new Error("SANDBOX_CONSUMER_SECRETS must be a JSON object");
  }
  return (consumerId) =>
    Object.hasOwn(secrets, consumerId) && typeof secrets[consumerId] === "string"
      ? secrets[consumerId]
      : undefined;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const { server, backend } = await startControlPlane();
  const address = server.address();
  console.log(`Sandbox control plane listening on http://${address.address}:${address.port}`);
  const shutdown = async () => {
    server.close();
    await backend.close();
  };
  process.once("SIGINT", shutdown);
  process.once("SIGTERM", shutdown);
}
