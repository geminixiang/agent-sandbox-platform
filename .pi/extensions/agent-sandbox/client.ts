import { createHmac } from "node:crypto";

export interface LocalCredentials {
  baseUrl: string;
  consumerId: string;
  subjectId: string;
  consumerSecret: string;
}

export function createSubjectToken(credentials: LocalCredentials, expiresAt = Math.floor(Date.now() / 1000) + 300): string {
  const payload = Buffer.from(JSON.stringify({ consumerId: credentials.consumerId, subjectId: credentials.subjectId, exp: expiresAt })).toString("base64url");
  const signed = `v1.${payload}`;
  const signature = createHmac("sha256", credentials.consumerSecret).update(signed).digest("base64url");
  return `${signed}.${signature}`;
}

export interface SandboxRecord {
  id: string;
  pool: string;
  status: "active" | "released" | "expired";
  createdAt: string;
  expiresAt: string;
  lastUsedAt: string;
}

export interface ExecResult { stdout: string; stderr: string; code: number }

export class PlatformClient {
  constructor(private readonly baseUrl: URL, private readonly token: () => string) {}

  async create(pool: string, ttlSeconds?: number, signal?: AbortSignal): Promise<SandboxRecord> {
    const payload = await this.request("v1/leases", { method: "POST", headers: { "idempotency-key": crypto.randomUUID() }, body: { pool, ttlSeconds }, signal });
    return payload.lease as SandboxRecord;
  }
  async get(id: string, signal?: AbortSignal): Promise<SandboxRecord> {
    const payload = await this.request(`v1/leases/${encodeURIComponent(id)}`, { signal });
    return payload.lease as SandboxRecord;
  }
  async exec(id: string, command: string, options: { cwd?: string; env?: Record<string,string>; timeoutSeconds?: number; signal?: AbortSignal } = {}): Promise<ExecResult> {
    const payload = await this.request(`v1/leases/${encodeURIComponent(id)}/exec`, { method: "POST", body: { command, cwd: options.cwd, env: options.env, timeoutSeconds: options.timeoutSeconds }, signal: options.signal });
    return { stdout: String(payload.stdout), stderr: String(payload.stderr), code: Number(payload.code) };
  }
  async writeFile(id: string, path: string, content: string, encoding: "utf8"|"base64", signal?: AbortSignal): Promise<void> {
    await this.request(`v1/leases/${encodeURIComponent(id)}/files/write`, { method: "POST", body: { path, content, encoding }, signal });
  }
  async readFile(id: string, path: string, encoding: "utf8"|"base64", signal?: AbortSignal): Promise<string> {
    const payload = await this.request(`v1/leases/${encodeURIComponent(id)}/files/read`, { method: "POST", body: { path, encoding }, signal });
    return String(payload.content);
  }
  async release(id: string, signal?: AbortSignal): Promise<SandboxRecord> {
    const payload = await this.request(`v1/leases/${encodeURIComponent(id)}/release`, { method: "POST", signal });
    return payload.lease as SandboxRecord;
  }
  async delete(id: string, signal?: AbortSignal): Promise<void> {
    await this.request(`v1/leases/${encodeURIComponent(id)}`, { method: "DELETE", signal });
  }

  private async request(path: string, options: { method?: string; headers?: Record<string,string>; body?: object; signal?: AbortSignal }): Promise<Record<string,unknown>> {
    const response = await fetch(new URL(path, this.baseUrl), {
      method: options.method ?? "GET",
      headers: { accept: "application/json", authorization: `Bearer ${this.token()}`, ...(options.body ? { "content-type": "application/json" } : {}), ...options.headers },
      body: options.body ? JSON.stringify(options.body) : undefined,
      signal: options.signal,
    });
    const text = await response.text();
    const payload = text ? JSON.parse(text) as Record<string,unknown> : {};
    if (!response.ok) {
      const envelope = payload.error as { code?: string; message?: string } | undefined;
      throw Object.assign(new Error(envelope?.message ?? `HTTP ${response.status}`), { code: envelope?.code, status: response.status });
    }
    return payload;
  }
}

export function resolveSecretEnvironment(mapping: Record<string,string> | undefined, environment: NodeJS.ProcessEnv = process.env): { values: Record<string,string>; secrets: string[] } {
  const values: Record<string,string> = {};
  const secrets: string[] = [];
  for (const [sandboxName, hostName] of Object.entries(mapping ?? {})) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(sandboxName)) throw new Error(`Invalid sandbox environment name '${sandboxName}'`);
    const value = environment[hostName];
    if (value === undefined) throw new Error(`Host environment variable '${hostName}' is not set`);
    values[sandboxName] = value;
    if (value) secrets.push(value);
  }
  return { values, secrets };
}

export function redact(value: string, secrets: string[]): string {
  return secrets.filter(Boolean).sort((a,b) => b.length-a.length).reduce((result, secret) => result.split(secret).join("[REDACTED]"), value);
}
