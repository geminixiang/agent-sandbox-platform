import { createHmac, randomUUID } from "node:crypto";

const LEASE_PATH = "/v1/leases";

export class SandboxPlatformError extends Error {
  constructor(message, options = {}) {
    super(message, options);
    this.name = "SandboxPlatformError";
    this.status = options.status;
    this.code = options.code;
  }
}

export class SandboxPlatformClient {
  constructor(options) {
    this.baseUrl = new URL(options.baseUrl);
    this.consumerId = requireIdentity(options.consumerId, "consumerId");
    this.subjectId = requireIdentity(options.subjectId, "subjectId");
    this.consumerSecret = options.consumerSecret;
    if (!this.consumerSecret) throw new TypeError("consumerSecret is required");
    this.fetch = options.fetch ?? globalThis.fetch;
    this.timeoutMs = options.timeoutMs ?? 30_000;
    this.tokenTtlSeconds = options.tokenTtlSeconds ?? 300;
    if (
      !Number.isInteger(this.tokenTtlSeconds) ||
      this.tokenTtlSeconds <= 0 ||
      this.tokenTtlSeconds > 300
    ) {
      throw new TypeError("tokenTtlSeconds must be an integer between 1 and 300");
    }
    if (typeof this.fetch !== "function") throw new TypeError("fetch is required");
  }

  async acquire(request, options = {}) {
    const idempotencyKey = options.idempotencyKey ?? randomUUID();
    const response = await this.request(LEASE_PATH, {
      method: "POST",
      headers: { "idempotency-key": idempotencyKey },
      body: request,
      signal: options.signal,
    });
    return {
      lease: new LeaseHandle(this, response.lease),
      replayed: response.replayed,
      idempotencyKey,
    };
  }

  async get(id, options) {
    const response = await this.request(`${LEASE_PATH}/${encodeURIComponent(id)}`, {
      signal: options?.signal,
    });
    return new LeaseHandle(this, response.lease);
  }

  request(path, options = {}) {
    return requestJson({
      baseUrl: this.baseUrl,
      token: createSubjectToken({
        consumerId: this.consumerId,
        subjectId: this.subjectId,
        consumerSecret: this.consumerSecret,
        expiresAt: Math.floor(Date.now() / 1000) + this.tokenTtlSeconds,
      }),
      fetch: this.fetch,
      timeoutMs: this.timeoutMs,
      path,
      ...options,
    });
  }
}

export class LeaseHandle {
  constructor(client, record) {
    this.client = client;
    this.record = record;
  }

  get id() {
    return this.record.id;
  }

  async refresh(options) {
    const handle = await this.client.get(this.id, options);
    this.record = handle.record;
    return this.record;
  }

  exec(command, options = {}) {
    return this.client.request(`${LEASE_PATH}/${encodeURIComponent(this.id)}/exec`, {
      method: "POST",
      body: {
        command,
        cwd: options.cwd,
        env: options.env,
        timeoutSeconds: options.timeoutSeconds,
      },
      signal: options.signal,
    });
  }

  async readFile(path, options = {}) {
    const response = await this.client.request(
      `${LEASE_PATH}/${encodeURIComponent(this.id)}/files/read`,
      {
        method: "POST",
        body: { path, encoding: options.encoding ?? "utf8" },
        signal: options.signal,
      },
    );
    return response.content;
  }

  writeFile(path, content, options = {}) {
    return this.client.request(`${LEASE_PATH}/${encodeURIComponent(this.id)}/files/write`, {
      method: "POST",
      body: { path, content, encoding: options.encoding ?? "utf8" },
      signal: options.signal,
    });
  }

  async release(options) {
    const response = await this.client.request(
      `${LEASE_PATH}/${encodeURIComponent(this.id)}/release`,
      { method: "POST", signal: options?.signal },
    );
    this.record = response.lease;
    return this.record;
  }

  async delete(options) {
    await this.client.request(`${LEASE_PATH}/${encodeURIComponent(this.id)}`, {
      method: "DELETE",
      signal: options?.signal,
    });
  }
}

export function createSubjectToken({ consumerId, subjectId, consumerSecret, expiresAt }) {
  requireIdentity(consumerId, "consumerId");
  requireIdentity(subjectId, "subjectId");
  if (!consumerSecret) throw new TypeError("consumerSecret is required");
  if (!Number.isInteger(expiresAt)) throw new TypeError("expiresAt must be an integer Unix timestamp");
  const payload = Buffer.from(JSON.stringify({ consumerId, subjectId, exp: expiresAt })).toString(
    "base64url",
  );
  const signed = `v1.${payload}`;
  const signature = createHmac("sha256", consumerSecret).update(signed).digest("base64url");
  return `${signed}.${signature}`;
}

async function requestJson({
  baseUrl,
  token,
  fetch,
  timeoutMs,
  path,
  method = "GET",
  headers,
  body,
  signal,
}) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(new Error("request timed out")), timeoutMs);
  const abort = () => controller.abort(signal.reason);
  if (signal?.aborted) abort();
  else signal?.addEventListener("abort", abort, { once: true });

  try {
    const response = await fetch(new URL(path, baseUrl), {
      method,
      headers: {
        accept: "application/json",
        authorization: `Bearer ${token}`,
        ...(body === undefined ? {} : { "content-type": "application/json" }),
        ...headers,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: controller.signal,
    });
    const text = await response.text();
    const parsed = text ? JSON.parse(text) : undefined;
    if (!response.ok) {
      throw new SandboxPlatformError(
        parsed?.error?.message ?? `Sandbox platform returned HTTP ${response.status}`,
        { status: response.status, code: parsed?.error?.code },
      );
    }
    return parsed;
  } catch (error) {
    if (error instanceof SandboxPlatformError) throw error;
    if (controller.signal.aborted) {
      throw new SandboxPlatformError("Sandbox platform request was aborted", { cause: error });
    }
    throw new SandboxPlatformError(`Sandbox platform request failed: ${error.message}`, {
      cause: error,
    });
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener("abort", abort);
  }
}

function requireIdentity(value, name) {
  if (typeof value !== "string" || !value.trim() || value.length > 200) {
    throw new TypeError(`${name} must be a non-empty string of at most 200 characters`);
  }
  return value;
}
