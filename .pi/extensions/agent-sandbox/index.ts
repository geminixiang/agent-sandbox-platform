import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { createSubjectToken, PlatformClient, redact, resolveSecretEnvironment, type LocalCredentials, type SandboxRecord } from "./client.js";

const sandboxId = Type.Optional(Type.String({ description: "Sandbox ID; defaults to this pi session's current sandbox" }));
const secretMapping = Type.Optional(Type.Record(Type.String(), Type.String(), { description: "Map sandbox variable names to host environment variable names. Secret values must never be passed directly." }));

export default function agentSandboxExtension(pi: ExtensionAPI) {
  let current: SandboxRecord | undefined;
  const owned = new Set<string>();
  const localCredentials = loadLocalCredentials(process.cwd());
  const baseUrl = new URL(process.env.SANDBOX_PLATFORM_URL ?? localCredentials?.baseUrl ?? "http://127.0.0.1:8787/");
  const client = new PlatformClient(baseUrl, () => {
    const token = process.env.SANDBOX_PLATFORM_TOKEN;
    if (token) return token;
    if (localCredentials) return createSubjectToken(localCredentials);
    throw new Error("Run ./scripts/local/pi-up.sh before starting pi, or set SANDBOX_PLATFORM_TOKEN");
  });
  const resolveId = (id?: string) => id ?? current?.id ?? (() => { throw new Error("No current sandbox; call sandbox_create first") })();
  const text = (value: unknown, secrets: string[] = []) => ({ content: [{ type: "text" as const, text: redact(typeof value === "string" ? value : JSON.stringify(value, null, 2), secrets) }], details: {} });

  pi.registerTool({
    name: "sandbox_create", label: "Create Sandbox", description: "Create an isolated platform Sandbox and make it current for this pi session.",
    promptSnippet: "Create an isolated Sandbox through the platform",
    promptGuidelines: ["Use sandbox_create before sandbox tools when this pi session has no current Sandbox. Never ask for secret values; sandbox_exec accepts host environment variable names."],
    parameters: Type.Object({ pool: Type.Optional(Type.String({ default: "coding" })), ttlSeconds: Type.Optional(Type.Integer({ minimum: 1 })) }),
    async execute(_id, params, signal) {
      if (current?.status === "active") throw new Error(`Current sandbox '${current.id}' is still active; close it first`);
      current = await client.create(params.pool ?? "coding", params.ttlSeconds, signal); owned.add(current.id); return text(current);
    },
  });

  pi.registerTool({
    name: "sandbox_status", label: "Sandbox Status", description: "Inspect the current or specified Sandbox without exposing infrastructure details.",
    parameters: Type.Object({ sandboxId }),
    async execute(_id, params, signal) { const record = await client.get(resolveId(params.sandboxId), signal); if (current?.id === record.id) current = record; return text(record); },
  });

  pi.registerTool({
    name: "sandbox_exec", label: "Sandbox Exec", description: "Execute a command inside an active Sandbox. Secrets are referenced by host environment variable name and redacted from results.",
    promptSnippet: "Run commands inside the current isolated Sandbox",
    promptGuidelines: ["Use sandbox_exec for commands that should run in the isolated Sandbox. In secretEnv, values are host environment variable names, never secret values."],
    parameters: Type.Object({ sandboxId, command: Type.String(), cwd: Type.Optional(Type.String()), timeoutSeconds: Type.Optional(Type.Integer({ minimum: 1 })), secretEnv: secretMapping }),
    async execute(_id, params, signal) { const { values, secrets } = resolveSecretEnvironment(params.secretEnv); return text(await client.exec(resolveId(params.sandboxId), params.command, { cwd: params.cwd, env: values, timeoutSeconds: params.timeoutSeconds, signal }), secrets); },
  });

  pi.registerTool({
    name: "sandbox_write_file", label: "Sandbox Write File", description: "Write UTF-8 text or base64 bytes into the Sandbox workspace.",
    parameters: Type.Object({ sandboxId, path: Type.String(), content: Type.String(), encoding: Type.Optional(Type.Union([Type.Literal("utf8"), Type.Literal("base64")], { default: "utf8" })) }),
    async execute(_id, params, signal) { await client.writeFile(resolveId(params.sandboxId), params.path, params.content, params.encoding ?? "utf8", signal); return text({ path: params.path, written: true }); },
  });

  pi.registerTool({
    name: "sandbox_read_file", label: "Sandbox Read File", description: "Read UTF-8 text or base64 bytes from the Sandbox workspace.",
    parameters: Type.Object({ sandboxId, path: Type.String(), encoding: Type.Optional(Type.Union([Type.Literal("utf8"), Type.Literal("base64")], { default: "utf8" })) }),
    async execute(_id, params, signal) { return text(await client.readFile(resolveId(params.sandboxId), params.path, params.encoding ?? "utf8", signal)); },
  });

  pi.registerTool({
    name: "sandbox_browser_run", label: "Sandbox Browser", description: "Run a Playwright JavaScript module already written into a browser-pool Sandbox. The module may import playwright-core from /opt/browser/node_modules.",
    promptSnippet: "Run Playwright browser automation inside a browser Sandbox",
    promptGuidelines: ["For browser automation, create pool browser, write a .mjs script, then use sandbox_browser_run. Browser scripts execute inside the Sandbox, not on the host."],
    parameters: Type.Object({ sandboxId, scriptPath: Type.String({ description: "Path under /workspace to a Playwright .mjs module" }), timeoutSeconds: Type.Optional(Type.Integer({ minimum: 1, default: 60 })), secretEnv: secretMapping }),
    async execute(_id, params, signal) {
      if (!params.scriptPath.startsWith("/workspace/")) throw new Error("scriptPath must be under /workspace");
      const { values, secrets } = resolveSecretEnvironment(params.secretEnv);
      const command = `test -e /workspace/node_modules || ln -s /opt/browser/node_modules /workspace/node_modules; node ${JSON.stringify(params.scriptPath)}`;
      return text(await client.exec(resolveId(params.sandboxId), command, { cwd: "/workspace", env: values, timeoutSeconds: params.timeoutSeconds ?? 60, signal }), secrets);
    },
  });

  pi.registerTool({
    name: "sandbox_close", label: "Close Sandbox", description: "Release or permanently delete the current or specified Sandbox.",
    parameters: Type.Object({ sandboxId, delete: Type.Optional(Type.Boolean({ default: false })) }),
    async execute(_id, params, signal) {
      const id = resolveId(params.sandboxId); let result: unknown;
      if (params.delete) { await client.delete(id, signal); result = { id, status: "deleted" }; } else result = await client.release(id, signal);
      owned.delete(id); if (current?.id === id) current = undefined; return text(result);
    },
  });

  pi.on("session_start", (_event, ctx) => { ctx.ui.setStatus("agent-sandbox", ctx.ui.theme.fg("accent", `Sandbox: ${baseUrl.host}`)); });
  pi.on("session_shutdown", async () => { for (const id of owned) await client.release(id).catch(() => client.delete(id).catch(() => undefined)); owned.clear(); });
}

function loadLocalCredentials(cwd: string): LocalCredentials | undefined {
  try {
    const value = JSON.parse(readFileSync(join(cwd, ".sandbox-platform", "local.json"), "utf8")) as Partial<LocalCredentials>;
    if (typeof value.baseUrl !== "string" || typeof value.consumerId !== "string" || typeof value.subjectId !== "string" || typeof value.consumerSecret !== "string") return undefined;
    return value as LocalCredentials;
  } catch {
    return undefined;
  }
}
