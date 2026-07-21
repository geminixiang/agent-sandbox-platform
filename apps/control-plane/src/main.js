import { pathToFileURL } from "node:url";
import { ProcessSandboxBackend } from "./backend/process-backend.js";
import { createControlPlaneServer } from "./server.js";

export async function startControlPlane(options = {}) {
  const backend = options.backend ?? new ProcessSandboxBackend();
  const server = createControlPlaneServer({
    backend,
    token: options.token ?? process.env.SANDBOX_API_TOKEN,
  });
  const host = options.host ?? process.env.SANDBOX_HOST ?? "127.0.0.1";
  const port = Number(options.port ?? process.env.SANDBOX_PORT ?? 8787);
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host, resolve);
  });
  return { server, backend };
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
