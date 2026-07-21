const SANDBOX_PATH = "/v1/sandboxes";

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
    this.token = options.token;
    this.fetch = options.fetch ?? globalThis.fetch;
    this.timeoutMs = options.timeoutMs ?? 30_000;
    if (typeof this.fetch !== "function") throw new TypeError("fetch is required");
  }

  async acquire(request, options) {
    const response = await this.request(`${SANDBOX_PATH}/acquire`, {
      method: "POST",
      body: request,
      signal: options?.signal,
    });
    return {
      sandbox: new SandboxHandle(this, response.sandbox),
      reused: response.reused,
    };
  }

  async get(id, options) {
    const response = await this.request(`${SANDBOX_PATH}/${encodeURIComponent(id)}`, {
      signal: options?.signal,
    });
    return new SandboxHandle(this, response.sandbox);
  }

  request(path, options = {}) {
    return requestJson({
      baseUrl: this.baseUrl,
      token: this.token,
      fetch: this.fetch,
      timeoutMs: this.timeoutMs,
      path,
      ...options,
    });
  }
}

export class SandboxHandle {
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
    return this.client.request(`${SANDBOX_PATH}/${encodeURIComponent(this.id)}/exec`, {
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
      `${SANDBOX_PATH}/${encodeURIComponent(this.id)}/files/read`,
      {
        method: "POST",
        body: { path, encoding: options.encoding ?? "utf8" },
        signal: options.signal,
      },
    );
    return response.content;
  }

  writeFile(path, content, options = {}) {
    return this.client.request(`${SANDBOX_PATH}/${encodeURIComponent(this.id)}/files/write`, {
      method: "POST",
      body: { path, content, encoding: options.encoding ?? "utf8" },
      signal: options.signal,
    });
  }

  async release(options) {
    const response = await this.client.request(
      `${SANDBOX_PATH}/${encodeURIComponent(this.id)}/release`,
      { method: "POST", signal: options?.signal },
    );
    this.record = response.sandbox;
    return this.record;
  }

  async delete(options) {
    await this.client.request(`${SANDBOX_PATH}/${encodeURIComponent(this.id)}`, {
      method: "DELETE",
      signal: options?.signal,
    });
  }
}

async function requestJson({ baseUrl, token, fetch, timeoutMs, path, method = "GET", body, signal }) {
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
        ...(body === undefined ? {} : { "content-type": "application/json" }),
        ...(token ? { authorization: `Bearer ${token}` } : {}),
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
