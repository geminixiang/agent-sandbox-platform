import { createHash, createHmac, randomUUID } from "node:crypto";

const LEASE_PATH = "/v1/leases";
const MAX_FILE_TRANSFER_BYTES = 64 * 1024 * 1024;
const SHA256_HEX = /^[0-9a-f]{64}$/;

export class SandboxPlatformError extends Error {
  constructor(message, options = {}) {
    super(message, options);
    this.name = "SandboxPlatformError";
    this.status = options.status;
    this.code = options.code;
  }
}

export class SandboxPlatformIntegrityError extends SandboxPlatformError {
  constructor(message, options = {}) {
    super(message, options);
    this.name = "SandboxPlatformIntegrityError";
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
      token: this.subjectToken(),
      fetch: this.fetch,
      timeoutMs: this.timeoutMs,
      path,
      ...options,
    });
  }

  subjectToken() {
    return createSubjectToken({
      consumerId: this.consumerId,
      subjectId: this.subjectId,
      consumerSecret: this.consumerSecret,
      expiresAt: Math.floor(Date.now() / 1000) + this.tokenTtlSeconds,
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

  readFileStream(path, options = {}) {
    return requestFileDownload({
      client: this.client,
      path: `${LEASE_PATH}/${encodeURIComponent(this.id)}/files/content?path=${encodeURIComponent(path)}`,
      signal: options.signal,
    });
  }

  writeFileStream(path, chunks, options) {
    if (options === undefined) throw new TypeError("streaming write options are required");
    return requestFileUpload({
      client: this.client,
      path: `${LEASE_PATH}/${encodeURIComponent(this.id)}/files/content?path=${encodeURIComponent(path)}`,
      chunks,
      sizeBytes: requireSize(options.sizeBytes),
      sha256: requireSHA256(options.sha256),
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

export class FileDownload {
  constructor({ response, reader, sizeBytes, sha256, requestContext }) {
    this.sizeBytes = sizeBytes;
    this.sha256 = sha256;
    this.response = response;
    this.reader = reader;
    this.requestContext = requestContext;
    this.iterated = false;
    this.closed = false;
  }

  async *[Symbol.asyncIterator]() {
    if (this.iterated) throw new TypeError("FileDownload can only be iterated once");
    this.iterated = true;
    const digest = createHash("sha256");
    let received = 0;
    let reachedEOF = false;
    try {
      while (true) {
        let result;
        try {
          result = await this.reader.read();
        } catch (error) {
          if (this.requestContext.controller.signal.aborted) {
            throw abortedError(error);
          }
          throw new SandboxPlatformIntegrityError("Streaming download ended before normal EOF", {
            cause: error,
            code: "CONTENT_LENGTH_MISMATCH",
          });
        }
        if (result.done) {
          reachedEOF = true;
          break;
        }
        const chunk = result.value;
        received += chunk.byteLength;
        digest.update(chunk);
        if (received > this.sizeBytes || received > MAX_FILE_TRANSFER_BYTES) {
          throw new SandboxPlatformIntegrityError("Streaming download length does not match Content-Length", {
            code: "CONTENT_LENGTH_MISMATCH",
          });
        }
        yield chunk;
      }
      if (received !== this.sizeBytes) {
        throw new SandboxPlatformIntegrityError("Streaming download length does not match Content-Length", {
          code: "CONTENT_LENGTH_MISMATCH",
        });
      }
      if (digest.digest("hex") !== this.sha256) {
        throw new SandboxPlatformIntegrityError("Streaming download does not match Content-Digest", {
          code: "CONTENT_DIGEST_MISMATCH",
        });
      }
    } finally {
      await this.close(reachedEOF);
    }
  }

  async close(reachedEOF = false) {
    if (this.closed) return;
    this.closed = true;
    if (!reachedEOF) {
      try {
        await this.reader.cancel("FileDownload closed before EOF");
      } catch {
        // The transport may already be closed by timeout or caller cancellation.
      }
    }
    this.requestContext.cleanup();
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

async function requestFileDownload({ client, path, signal }) {
  const requestContext = createRequestContext(client.timeoutMs, signal);
  let response;
  try {
    response = await client.fetch(new URL(path, client.baseUrl), {
      headers: {
        accept: "application/octet-stream",
        authorization: `Bearer ${client.subjectToken()}`,
      },
      signal: requestContext.controller.signal,
    });
    if (!response.ok) {
      const error = await platformErrorFromResponse(response);
      requestContext.cleanup();
      throw error;
    }
    const contentType = response.headers.get("content-type")?.split(";", 1)[0].trim();
    const sizeBytes = parseResponseSize(response.headers.get("content-length"));
    const sha256 = parseContentDigest(response.headers.get("content-digest"));
    if (contentType !== "application/octet-stream" || response.body === null) {
      requestContext.cleanup();
      throw new SandboxPlatformIntegrityError("Sandbox platform returned invalid streaming metadata", {
        code: "INVALID_STREAMING_RESPONSE",
      });
    }
    return new FileDownload({
      response,
      reader: response.body.getReader(),
      sizeBytes,
      sha256,
      requestContext,
    });
  } catch (error) {
    if (response?.body !== null) {
      try { await response?.body?.cancel(); } catch { /* already closed */ }
    }
    requestContext.cleanup();
    if (error instanceof SandboxPlatformError) throw error;
    if (requestContext.controller.signal.aborted) throw abortedError(error);
    throw new SandboxPlatformError(`Sandbox platform request failed: ${error.message}`, {
      cause: error,
    });
  }
}

async function requestFileUpload({ client, path, chunks, sizeBytes, sha256, signal }) {
  if (chunks === null || typeof chunks?.[Symbol.asyncIterator] !== "function") {
    throw new TypeError("chunks must be an AsyncIterable<Uint8Array>");
  }
  const requestContext = createRequestContext(client.timeoutMs, signal);
  try {
    const response = await client.fetch(new URL(path, client.baseUrl), {
      method: "PUT",
      headers: {
        accept: "application/json",
        authorization: `Bearer ${client.subjectToken()}`,
        "content-type": "application/octet-stream",
        "content-length": String(sizeBytes),
        "content-digest": formatContentDigest(sha256),
      },
      body: verifiedUpload(chunks, sizeBytes, sha256),
      duplex: "half",
      signal: requestContext.controller.signal,
    });
    if (!response.ok) throw await platformErrorFromResponse(response);
    if (response.status !== 204) {
      throw new SandboxPlatformError(`Sandbox platform returned unexpected HTTP ${response.status}`, {
        status: response.status,
        code: "INVALID_STREAMING_RESPONSE",
      });
    }
  } catch (error) {
    if (error instanceof SandboxPlatformError) throw error;
    if (requestContext.controller.signal.aborted) throw abortedError(error);
    throw new SandboxPlatformError(`Sandbox platform request failed: ${error.message}`, {
      cause: error,
    });
  } finally {
    requestContext.cleanup();
  }
}

async function* verifiedUpload(chunks, sizeBytes, expectedSHA256) {
  const digest = createHash("sha256");
  let sent = 0;
  for await (const chunk of chunks) {
    if (!(chunk instanceof Uint8Array)) {
      throw new TypeError("streaming file chunks must be Uint8Array values");
    }
    sent += chunk.byteLength;
    if (sent > sizeBytes || sent > MAX_FILE_TRANSFER_BYTES) {
      throw new SandboxPlatformIntegrityError("Streaming upload length does not match sizeBytes", {
        code: "CONTENT_LENGTH_MISMATCH",
      });
    }
    digest.update(chunk);
    yield chunk;
  }
  if (sent !== sizeBytes) {
    throw new SandboxPlatformIntegrityError("Streaming upload length does not match sizeBytes", {
      code: "CONTENT_LENGTH_MISMATCH",
    });
  }
  if (digest.digest("hex") !== expectedSHA256) {
    throw new SandboxPlatformIntegrityError("Streaming upload does not match sha256", {
      code: "CONTENT_DIGEST_MISMATCH",
    });
  }
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
  const requestContext = createRequestContext(timeoutMs, signal);
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
      signal: requestContext.controller.signal,
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
    if (requestContext.controller.signal.aborted) throw abortedError(error);
    throw new SandboxPlatformError(`Sandbox platform request failed: ${error.message}`, {
      cause: error,
    });
  } finally {
    requestContext.cleanup();
  }
}

function createRequestContext(timeoutMs, signal) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(new Error("request timed out")), timeoutMs);
  const abort = () => controller.abort(signal.reason);
  if (signal?.aborted) abort();
  else signal?.addEventListener("abort", abort, { once: true });
  let cleaned = false;
  return {
    controller,
    cleanup() {
      if (cleaned) return;
      cleaned = true;
      clearTimeout(timeout);
      signal?.removeEventListener("abort", abort);
    },
  };
}

async function platformErrorFromResponse(response) {
  let parsed;
  try {
    const text = await response.text();
    parsed = text ? JSON.parse(text) : undefined;
  } catch {
    parsed = undefined;
  }
  return new SandboxPlatformError(
    parsed?.error?.message ?? `Sandbox platform returned HTTP ${response.status}`,
    { status: response.status, code: parsed?.error?.code },
  );
}

function parseResponseSize(value) {
  if (value === null || !/^(0|[1-9][0-9]*)$/.test(value)) {
    throw new SandboxPlatformIntegrityError("Sandbox platform returned invalid Content-Length", {
      code: "INVALID_STREAMING_RESPONSE",
    });
  }
  const result = Number(value);
  if (!Number.isSafeInteger(result) || result > MAX_FILE_TRANSFER_BYTES) {
    throw new SandboxPlatformIntegrityError("Sandbox platform returned an unsupported Content-Length", {
      code: "TRANSFER_TOO_LARGE",
    });
  }
  return result;
}

function parseContentDigest(value) {
  const match = /^sha-256=:([A-Za-z0-9+/]{43}=):$/.exec(value ?? "");
  if (match === null) {
    throw new SandboxPlatformIntegrityError("Sandbox platform returned invalid Content-Digest", {
      code: "INVALID_CONTENT_DIGEST",
    });
  }
  const bytes = Buffer.from(match[1], "base64");
  if (bytes.byteLength !== 32 || bytes.toString("base64") !== match[1]) {
    throw new SandboxPlatformIntegrityError("Sandbox platform returned invalid Content-Digest", {
      code: "INVALID_CONTENT_DIGEST",
    });
  }
  return bytes.toString("hex");
}

function formatContentDigest(sha256) {
  return `sha-256=:${Buffer.from(sha256, "hex").toString("base64")}:`;
}

function requireSize(value) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TypeError("sizeBytes must be a non-negative safe integer");
  }
  if (value > MAX_FILE_TRANSFER_BYTES) {
    throw new SandboxPlatformError("File transfer exceeds the 64 MiB limit", {
      code: "TRANSFER_TOO_LARGE",
    });
  }
  return value;
}

function requireSHA256(value) {
  if (typeof value !== "string" || !SHA256_HEX.test(value)) {
    throw new TypeError("sha256 must be a lowercase 64-character hexadecimal digest");
  }
  return value;
}

function abortedError(cause) {
  return new SandboxPlatformError("Sandbox platform request was aborted", {
    cause,
    code: "ABORTED",
  });
}

function requireIdentity(value, name) {
  if (typeof value !== "string" || !value.trim() || value.length > 200) {
    throw new TypeError(`${name} must be a non-empty string of at most 200 characters`);
  }
  return value;
}
