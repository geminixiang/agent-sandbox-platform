import { createHash, createHmac, randomUUID } from "node:crypto";

const LEASE_PATH = "/v1/leases";
const MAX_FILE_TRANSFER_BYTES = 64 * 1024 * 1024;
const SHA256_HEX = /^[0-9a-f]{64}$/;

export class SandboxError extends Error {
  constructor(message, options = {}) {
    super(message, { cause: options.cause });
    this.name = new.target.name;
    this.status = options.status;
    this.code = options.code;
  }
}

/** @deprecated Use SandboxError. */
export const SandboxPlatformError = SandboxError;

export class CommandFailedError extends SandboxError {
  constructor(command, result) {
    super(`command exited with status ${result.exitCode}`, { code: "COMMAND_FAILED" });
    this.command = command;
    this.result = result;
  }
}

export class SandboxNotFoundError extends SandboxError {}
export class SandboxNotActiveError extends SandboxError {}
export class SandboxExpiredError extends SandboxNotActiveError {}
export class SandboxQuotaExceededError extends SandboxError {}
export class SandboxAbortedError extends SandboxError {}
export class SandboxFileNotFoundError extends SandboxError {}
export class SandboxTransferTooLargeError extends SandboxError {}
export class SandboxTransferLimitError extends SandboxError {}
export class SandboxIntegrityError extends SandboxError {}
export class SandboxStreamingNotSupportedError extends SandboxError {}
export class SandboxInvalidCursorError extends SandboxError {}
export class SandboxCursorExpiredError extends SandboxError {}
export class SandboxUnknownPoolError extends SandboxError {}

/** @deprecated Use SandboxIntegrityError. */
export const SandboxPlatformIntegrityError = SandboxIntegrityError;

export class StaticToken {
  constructor(token) {
    this.token = requireNonEmpty(token, "token");
    Object.freeze(this);
  }

  getToken() {
    return this.token;
  }
}

export class CommandResult {
  constructor({ stdout, stderr, code }) {
    this.stdout = requireString(stdout, "stdout");
    this.stderr = requireString(stderr, "stderr");
    this.exitCode = requireInteger(code, "code");
    Object.freeze(this);
  }

  /** @deprecated Use exitCode. */
  get code() {
    return this.exitCode;
  }

  get succeeded() {
    return this.exitCode === 0;
  }
}

export class SandboxPage {
  constructor(sandboxes, nextCursor) {
    const frozenSandboxes = Object.freeze([...sandboxes]);
    this.sandboxes = frozenSandboxes;
    this.nextCursor = nextCursor;
    Object.defineProperty(this, "leases", {
      enumerable: true,
      value: frozenSandboxes,
    });
    Object.freeze(this);
  }
}

export class SandboxClient {
  constructor(options) {
    if (options === null || typeof options !== "object") {
      throw new TypeError("SandboxClient options are required");
    }
    this.baseUrl = new URL(options.baseUrl);
    this.fetch = options.fetch ?? globalThis.fetch;
    if (typeof this.fetch !== "function") throw new TypeError("fetch is required");
    this.timeoutMs = requireTimeout(options.timeoutMs ?? 30_000);
    this.closed = false;

    if (options.credentials !== undefined) {
      this.credentials = normalizeCredentials(options.credentials);
      if (options.credentials instanceof StaticToken) {
        this.subjectTokenProvider = () => options.credentials.getToken();
      }
    } else {
      const consumerId = requireIdentity(options.consumerId, "consumerId");
      const subjectId = requireIdentity(options.subjectId, "subjectId");
      const consumerSecret = requireNonEmpty(options.consumerSecret, "consumerSecret");
      const tokenTtlSeconds = options.tokenTtlSeconds ?? 300;
      if (!Number.isInteger(tokenTtlSeconds) || tokenTtlSeconds <= 0 || tokenTtlSeconds > 300) {
        throw new TypeError("tokenTtlSeconds must be an integer between 1 and 300");
      }
      this.subjectTokenProvider = () => createSubjectToken({
        consumerId,
        subjectId,
        consumerSecret,
        expiresAt: Math.floor(Date.now() / 1000) + tokenTtlSeconds,
      });
      this.credentials = this.subjectTokenProvider;
    }
  }

  async close() {
    this.closed = true;
  }

  async [Symbol.asyncDispose]() {
    await this.close();
  }

  async create(options) {
    return (await this.#acquire(options, options)).sandbox;
  }

  async sandbox(options, callback) {
    if (typeof callback !== "function") throw new TypeError("sandbox callback is required");
    const sandbox = await this.create(options);
    let callbackResult;
    let callbackError;
    let callbackFailed = false;
    try {
      callbackResult = await callback(sandbox);
    } catch (error) {
      callbackFailed = true;
      callbackError = error;
    }

    let cleanupError;
    let cleanupFailed = false;
    try {
      await sandbox.close();
    } catch (error) {
      cleanupFailed = true;
      cleanupError = error;
    }

    if (callbackFailed && cleanupFailed) {
      throw new AggregateError(
        [callbackError, cleanupError],
        "Sandbox callback and cleanup both failed",
      );
    }
    if (callbackFailed) throw callbackError;
    if (cleanupFailed) throw cleanupError;
    return callbackResult;
  }

  /** @deprecated Use create. */
  async acquire(request, options = {}) {
    const acquired = await this.#acquire(request, options);
    return Object.freeze({
      lease: acquired.sandbox,
      replayed: acquired.replayed,
      idempotencyKey: acquired.idempotencyKey,
    });
  }

  async #acquire(request, operationOptions = {}) {
    if (request === null || typeof request !== "object") {
      throw new TypeError("create options are required");
    }
    const idempotencyKey = operationOptions.idempotencyKey ?? request.idempotencyKey ?? randomUUID();
    const body = { pool: requireNonEmpty(request.pool, "pool") };
    if (request.ttlSeconds !== undefined) body.ttlSeconds = request.ttlSeconds;
    const response = await this.request(LEASE_PATH, {
      method: "POST",
      headers: { "idempotency-key": requireNonEmpty(idempotencyKey, "idempotencyKey") },
      body,
      signal: operationOptions.signal,
      timeoutMs: operationOptions.timeoutMs,
    });
    return {
      sandbox: new Sandbox(this, response.lease),
      replayed: Boolean(response.replayed),
      idempotencyKey,
    };
  }

  async listPage(options = {}) {
    const limit = options.limit ?? 50;
    if (!Number.isInteger(limit) || limit < 1 || limit > 100) {
      throw new TypeError("limit must be an integer between 1 and 100");
    }
    const query = new URLSearchParams({ limit: String(limit) });
    if (options.pool !== undefined) query.set("pool", requireNonEmpty(options.pool, "pool"));
    if (options.cursor !== undefined) query.set("cursor", requireNonEmpty(options.cursor, "cursor"));
    const response = await this.request(`${LEASE_PATH}?${query}`, options);
    if (
      !Array.isArray(response?.leases) ||
      (response.nextCursor !== null &&
        (typeof response.nextCursor !== "string" || response.nextCursor.length === 0))
    ) {
      throw new SandboxError("Sandbox platform returned an invalid list response");
    }
    return new SandboxPage(
      response.leases.map((record) => new Sandbox(this, record)),
      response.nextCursor,
    );
  }

  async *list(options = {}) {
    let cursor = options.cursor;
    const seen = new Set(cursor === undefined ? [] : [cursor]);
    for (;;) {
      const page = await this.listPage({ ...options, cursor });
      if (page.nextCursor !== null && seen.has(page.nextCursor)) {
        throw new SandboxInvalidCursorError("Sandbox platform returned a repeated list cursor", {
          code: "INVALID_CURSOR",
        });
      }
      for (const sandbox of page.sandboxes) yield sandbox;
      if (page.nextCursor === null) return;
      seen.add(page.nextCursor);
      cursor = page.nextCursor;
    }
  }

  async connect(id, options = {}) {
    return this.get(id, options);
  }

  async get(id, options = {}) {
    const response = await this.request(`${LEASE_PATH}/${encodeURIComponent(requireNonEmpty(id, "id"))}`, options);
    return new Sandbox(this, response.lease);
  }

  async request(path, options = {}) {
    if (this.closed) throw new SandboxError("Sandbox client is closed", { code: "CLIENT_CLOSED" });
    const requestContext = createRequestContext(options.timeoutMs ?? this.timeoutMs, options.signal);
    try {
      const token = await resolveToken(this.credentials, requestContext.controller.signal);
      return await requestJson({
        baseUrl: this.baseUrl,
        token,
        fetch: this.fetch,
        path,
        method: options.method,
        headers: options.headers,
        body: options.body,
        signal: requestContext.controller.signal,
      });
    } catch (error) {
      throw normalizeOperationError(error, requestContext.controller.signal);
    } finally {
      requestContext.cleanup();
    }
  }

  /** @deprecated Tokens are now supplied by credentials. */
  subjectToken() {
    if (this.subjectTokenProvider === undefined) {
      throw new TypeError("subjectToken is unavailable for dynamic credentials");
    }
    return this.subjectTokenProvider();
  }
}

/** @deprecated Use SandboxClient. */
export const SandboxPlatformClient = SandboxClient;

export class Sandbox {
  constructor(client, record) {
    this.client = client;
    this.record = freezeRecord(record);
    this.files = Object.freeze(new SandboxFiles(this));
    this.closed = false;
    this.transitionKind = undefined;
    this.transitionPromise = undefined;
  }

  get id() {
    return this.record.id;
  }

  async refresh(options = {}) {
    const current = await this.client.get(this.id, options);
    this.record = current.record;
    return this.record;
  }

  async run(command, options = {}) {
    const response = await this.client.request(`${LEASE_PATH}/${encodeURIComponent(this.id)}/exec`, {
      method: "POST",
      body: {
        command: requireNonEmpty(command, "command"),
        cwd: options.cwd,
        env: options.env,
        timeoutSeconds: options.timeoutSeconds,
      },
      signal: options.signal,
      timeoutMs: options.timeoutMs,
    });
    const result = new CommandResult(response);
    if (options.check && !result.succeeded) throw new CommandFailedError(command, result);
    return result;
  }

  /** @deprecated Use run. */
  async exec(command, options = {}) {
    const result = await this.run(command, options);
    return Object.freeze({ stdout: result.stdout, stderr: result.stderr, code: result.code });
  }

  /** @deprecated Use files.readText or files.readBytes. */
  readFile(path, options = {}) {
    return this.files.readFile(path, options);
  }

  /** @deprecated Use files.writeText or files.writeBytes. */
  writeFile(path, content, options = {}) {
    return this.files.writeFile(path, content, options);
  }

  /** @deprecated Use files.readStream. */
  readFileStream(path, options = {}) {
    return this.files.readStream(path, options);
  }

  /** @deprecated Use files.writeStream. */
  writeFileStream(path, chunks, options) {
    return this.files.writeStream(path, chunks, options);
  }

  async release(options = {}) {
    if (this.transitionPromise === undefined) {
      this.transitionKind = "release";
      this.transitionPromise = (async () => {
        const response = await this.client.request(`${LEASE_PATH}/${encodeURIComponent(this.id)}/release`, {
          method: "POST",
          signal: options.signal,
          timeoutMs: options.timeoutMs,
        });
        this.record = freezeRecord(response.lease);
        this.closed = true;
        return this.record;
      })();
    }
    const result = await this.transitionPromise;
    return this.transitionKind === "release" ? result : this.record;
  }

  async delete(options = {}) {
    if (this.transitionPromise === undefined) {
      this.transitionKind = "delete";
      this.transitionPromise = (async () => {
        await this.client.request(`${LEASE_PATH}/${encodeURIComponent(this.id)}`, {
          method: "DELETE",
          signal: options.signal,
          timeoutMs: options.timeoutMs,
        });
        this.closed = true;
      })();
    }
    await this.transitionPromise;
  }

  async close(options = {}) {
    if (this.transitionPromise === undefined) {
      await this.release(options);
      return;
    }
    await this.transitionPromise;
  }

  async [Symbol.asyncDispose]() {
    await this.close();
  }
}

/** @deprecated Use Sandbox. */
export const LeaseHandle = Sandbox;

export class SandboxFiles {
  constructor(sandbox) {
    this.sandbox = sandbox;
  }

  async readText(path, options = {}) {
    return this.#read(path, "utf8", options);
  }

  async writeText(path, content, options = {}) {
    requireString(content, "content");
    await this.#write(path, content, "utf8", options);
  }

  async readBytes(path, options = {}) {
    return decodeCanonicalBase64(await this.#read(path, "base64", options));
  }

  async writeBytes(path, content, options = {}) {
    if (!(content instanceof Uint8Array)) throw new TypeError("content must be a Uint8Array");
    await this.#write(path, Buffer.from(content).toString("base64"), "base64", options);
  }

  readStream(path, options = {}) {
    return requestFileDownload({
      client: this.sandbox.client,
      path: `${LEASE_PATH}/${encodeURIComponent(this.sandbox.id)}/files/content?path=${encodeURIComponent(requireNonEmpty(String(path), "path"))}`,
      signal: options.signal,
      timeoutMs: options.timeoutMs,
    });
  }

  writeStream(path, chunks, options) {
    if (options === undefined) throw new TypeError("streaming write options are required");
    return requestFileUpload({
      client: this.sandbox.client,
      path: `${LEASE_PATH}/${encodeURIComponent(this.sandbox.id)}/files/content?path=${encodeURIComponent(requireNonEmpty(String(path), "path"))}`,
      chunks,
      sizeBytes: requireSize(options.sizeBytes),
      sha256: requireSHA256(options.sha256),
      signal: options.signal,
      timeoutMs: options.timeoutMs,
    });
  }

  /** @deprecated Use readText or readBytes. */
  #readLegacy(path, options = {}) {
    return this.#read(path, options.encoding ?? "utf8", options);
  }

  /** @deprecated Use readText or readBytes. */
  readFile(path, options = {}) {
    return this.#readLegacy(path, options);
  }

  /** @deprecated Use writeText or writeBytes. */
  writeFile(path, content, options = {}) {
    return this.#write(path, content, options.encoding ?? "utf8", options);
  }

  /** @deprecated Use readStream. */
  readFileStream(path, options = {}) {
    return this.readStream(path, options);
  }

  /** @deprecated Use writeStream. */
  writeFileStream(path, chunks, options) {
    return this.writeStream(path, chunks, options);
  }

  async #read(path, encoding, options) {
    const response = await this.sandbox.client.request(
      `${LEASE_PATH}/${encodeURIComponent(this.sandbox.id)}/files/read`,
      {
        method: "POST",
        body: { path: requireNonEmpty(String(path), "path"), encoding },
        signal: options.signal,
        timeoutMs: options.timeoutMs,
      },
    );
    return requireString(response.content, "content");
  }

  async #write(path, content, encoding, options) {
    await this.sandbox.client.request(`${LEASE_PATH}/${encodeURIComponent(this.sandbox.id)}/files/write`, {
      method: "POST",
      body: { path: requireNonEmpty(String(path), "path"), content, encoding },
      signal: options.signal,
      timeoutMs: options.timeoutMs,
    });
  }
}

export class FileDownload {
  constructor({ reader, sizeBytes, sha256, requestContext }) {
    this.sizeBytes = sizeBytes;
    this.sha256 = sha256;
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
          result = await awaitWithAbort(this.reader.read(), this.requestContext.controller.signal);
        } catch (error) {
          if (this.requestContext.controller.signal.aborted) throw abortedError(error);
          throw new SandboxIntegrityError("Streaming download ended before normal EOF", {
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
          throw new SandboxIntegrityError("Streaming download length does not match Content-Length", {
            code: "CONTENT_LENGTH_MISMATCH",
          });
        }
        yield chunk;
      }
      if (received !== this.sizeBytes) {
        throw new SandboxIntegrityError("Streaming download length does not match Content-Length", {
          code: "CONTENT_LENGTH_MISMATCH",
        });
      }
      if (digest.digest("hex") !== this.sha256) {
        throw new SandboxIntegrityError("Streaming download does not match Content-Digest", {
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
        // The transport may already be closed by timeout or cancellation.
      }
    }
    this.requestContext.cleanup();
  }

  async [Symbol.asyncDispose]() {
    await this.close();
  }
}

export function createSubjectToken({ consumerId, subjectId, consumerSecret, expiresAt }) {
  requireIdentity(consumerId, "consumerId");
  requireIdentity(subjectId, "subjectId");
  requireNonEmpty(consumerSecret, "consumerSecret");
  if (!Number.isInteger(expiresAt)) throw new TypeError("expiresAt must be an integer Unix timestamp");
  const payload = Buffer.from(JSON.stringify({ consumerId, subjectId, exp: expiresAt })).toString("base64url");
  const signed = `v1.${payload}`;
  const signature = createHmac("sha256", consumerSecret).update(signed).digest("base64url");
  return `${signed}.${signature}`;
}

async function requestFileDownload({ client, path, signal, timeoutMs }) {
  if (client.closed) throw new SandboxError("Sandbox client is closed", { code: "CLIENT_CLOSED" });
  const requestContext = createRequestContext(timeoutMs ?? client.timeoutMs, signal);
  let response;
  try {
    const token = await resolveToken(client.credentials, requestContext.controller.signal);
    response = await awaitWithAbort(client.fetch(new URL(path, client.baseUrl), {
      headers: { accept: "application/octet-stream", authorization: `Bearer ${token}` },
      signal: requestContext.controller.signal,
    }), requestContext.controller.signal);
    if (!response.ok) throw await platformErrorFromResponse(response, requestContext.controller.signal);
    const contentType = response.headers.get("content-type")?.split(";", 1)[0].trim();
    const sizeBytes = parseResponseSize(response.headers.get("content-length"));
    const sha256 = parseContentDigest(response.headers.get("content-digest"));
    if (contentType !== "application/octet-stream" || response.body === null) {
      throw new SandboxIntegrityError("Sandbox platform returned invalid streaming metadata", {
        code: "INVALID_STREAMING_RESPONSE",
      });
    }
    return new FileDownload({ reader: response.body.getReader(), sizeBytes, sha256, requestContext });
  } catch (error) {
    try {
      await response?.body?.cancel();
    } catch {
      // The response may already be closed.
    }
    requestContext.cleanup();
    throw normalizeOperationError(error, requestContext.controller.signal);
  }
}

async function requestFileUpload({ client, path, chunks, sizeBytes, sha256, signal, timeoutMs }) {
  if (chunks === null || typeof chunks?.[Symbol.asyncIterator] !== "function") {
    throw new TypeError("chunks must be an AsyncIterable<Uint8Array>");
  }
  if (client.closed) throw new SandboxError("Sandbox client is closed", { code: "CLIENT_CLOSED" });
  const requestContext = createRequestContext(timeoutMs ?? client.timeoutMs, signal);
  const uploadState = { completed: false, sourceFailed: false, sourceError: undefined };
  try {
    const token = await resolveToken(client.credentials, requestContext.controller.signal);
    const response = await awaitWithAbort(client.fetch(new URL(path, client.baseUrl), {
      method: "PUT",
      headers: {
        accept: "application/json",
        authorization: `Bearer ${token}`,
        "content-type": "application/octet-stream",
        "content-length": String(sizeBytes),
        "content-digest": formatContentDigest(sha256),
      },
      body: verifiedUpload(chunks, sizeBytes, sha256, uploadState),
      duplex: "half",
      signal: requestContext.controller.signal,
    }), requestContext.controller.signal);
    if (!response.ok) throw await platformErrorFromResponse(response, requestContext.controller.signal);
    if (response.status !== 204) {
      throw new SandboxError(`Sandbox platform returned unexpected HTTP ${response.status}`, {
        status: response.status,
        code: "INVALID_STREAMING_RESPONSE",
      });
    }
  } catch (error) {
    if (requestContext.controller.signal.aborted) throw abortedError(error);
    if (uploadState.sourceFailed) throw uploadState.sourceError;
    if (error instanceof SandboxError) throw error;
    if (uploadState.completed && error instanceof TypeError) {
      throw abortedError(error, "Streaming upload ended before the platform responded");
    }
    throw new SandboxError("Sandbox platform request failed", { cause: error });
  } finally {
    requestContext.cleanup();
  }
}

async function* verifiedUpload(chunks, sizeBytes, expectedSHA256, state) {
  const digest = createHash("sha256");
  let sent = 0;
  try {
    for await (const chunk of chunks) {
      if (!(chunk instanceof Uint8Array)) throw new TypeError("streaming file chunks must be Uint8Array values");
      sent += chunk.byteLength;
      if (sent > sizeBytes || sent > MAX_FILE_TRANSFER_BYTES) {
        throw new SandboxIntegrityError("Streaming upload length does not match sizeBytes", {
          code: "CONTENT_LENGTH_MISMATCH",
        });
      }
      digest.update(chunk);
      yield chunk;
    }
    if (sent !== sizeBytes) {
      throw new SandboxIntegrityError("Streaming upload length does not match sizeBytes", {
        code: "CONTENT_LENGTH_MISMATCH",
      });
    }
    if (digest.digest("hex") !== expectedSHA256) {
      throw new SandboxIntegrityError("Streaming upload does not match sha256", {
        code: "CONTENT_DIGEST_MISMATCH",
      });
    }
    state.completed = true;
  } catch (error) {
    state.sourceFailed = true;
    state.sourceError = error;
    throw error;
  }
}

async function requestJson({ baseUrl, token, fetch, path, method = "GET", headers, body, signal }) {
  const response = await awaitWithAbort(fetch(new URL(path, baseUrl), {
    method,
    headers: {
      accept: "application/json",
      authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { "content-type": "application/json" }),
      ...headers,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  }), signal);
  let parsed;
  try {
    const text = await awaitWithAbort(response.text(), signal);
    parsed = text ? JSON.parse(text) : undefined;
  } catch (error) {
    if (signal.aborted) throw error;
    throw new SandboxError("Sandbox platform returned an invalid JSON response", {
      cause: error,
      status: response.status,
    });
  }
  if (!response.ok) {
    throw platformError(parsed?.error?.message ?? `Sandbox platform returned HTTP ${response.status}`, {
      status: response.status,
      code: parsed?.error?.code,
    });
  }
  return parsed;
}

function createRequestContext(timeoutMs, signal) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(new Error("operation timed out")), requireTimeout(timeoutMs));
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

function normalizeCredentials(credentials) {
  if (typeof credentials === "function") return (context) => credentials(context);
  if (credentials !== null && typeof credentials?.getToken === "function") {
    return (context) => credentials.getToken(context);
  }
  throw new TypeError("credentials must be a token provider or StaticToken");
}

async function resolveToken(provider, signal) {
  if (signal.aborted) throw abortedError(signal.reason);
  let value;
  try {
    value = provider({ signal });
  } catch {
    throw credentialsFailedError();
  }
  try {
    const token = await awaitWithAbort(Promise.resolve(value), signal);
    if (typeof token !== "string" || !token.trim()) throw credentialsFailedError();
    return token;
  } catch (error) {
    if (signal.aborted) throw abortedError(error);
    throw credentialsFailedError();
  }
}

function credentialsFailedError() {
  return new SandboxError("Sandbox credentials could not be resolved", {
    code: "CREDENTIALS_FAILED",
  });
}

function awaitWithAbort(promise, signal) {
  return new Promise((resolve, reject) => {
    const abort = () => reject(signal.reason ?? new Error("operation aborted"));
    if (signal.aborted) return abort();
    signal.addEventListener("abort", abort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", abort);
        resolve(value);
      },
      (error) => {
        signal.removeEventListener("abort", abort);
        reject(error);
      },
    );
  });
}

async function platformErrorFromResponse(response, signal) {
  let parsed;
  try {
    const text = await awaitWithAbort(response.text(), signal);
    parsed = text ? JSON.parse(text) : undefined;
  } catch (error) {
    if (signal.aborted) throw error;
    parsed = undefined;
  }
  return platformError(parsed?.error?.message ?? `Sandbox platform returned HTTP ${response.status}`, {
    status: response.status,
    code: parsed?.error?.code,
  });
}

function platformError(message, options) {
  const ErrorType = {
    LEASE_NOT_FOUND: SandboxNotFoundError,
    LEASE_NOT_ACTIVE: SandboxNotActiveError,
    LEASE_EXPIRED: SandboxExpiredError,
    LEASE_QUOTA_EXCEEDED: SandboxQuotaExceededError,
    ABORTED: SandboxAbortedError,
    FILE_NOT_FOUND: SandboxFileNotFoundError,
    TRANSFER_TOO_LARGE: SandboxTransferTooLargeError,
    TRANSFER_LIMIT_REACHED: SandboxTransferLimitError,
    CONTENT_LENGTH_MISMATCH: SandboxIntegrityError,
    CONTENT_DIGEST_MISMATCH: SandboxIntegrityError,
    INVALID_CONTENT_DIGEST: SandboxIntegrityError,
    INVALID_STREAMING_RESPONSE: SandboxIntegrityError,
    STREAMING_NOT_SUPPORTED: SandboxStreamingNotSupportedError,
    INVALID_CURSOR: SandboxInvalidCursorError,
    CURSOR_EXPIRED: SandboxCursorExpiredError,
    UNKNOWN_POOL: SandboxUnknownPoolError,
  }[options.code] ?? SandboxError;
  return new ErrorType(message, options);
}

function normalizeOperationError(error, signal) {
  if (error instanceof SandboxError) return error;
  if (signal.aborted) return abortedError(error);
  if (error instanceof TypeError && error.message === "token must be a non-empty string") return error;
  return new SandboxError("Sandbox platform request failed", { cause: error });
}

function abortedError(cause, message = "Sandbox platform request was aborted") {
  return new SandboxAbortedError(message, { cause, code: "ABORTED" });
}

function freezeRecord(record) {
  if (record === null || typeof record !== "object") {
    throw new SandboxError("Sandbox platform returned an invalid lease record");
  }
  return Object.freeze({
    id: requireString(record.id, "id"),
    pool: requireString(record.pool, "pool"),
    status: requireLeaseStatus(record.status),
    createdAt: requireString(record.createdAt, "createdAt"),
    expiresAt: requireString(record.expiresAt, "expiresAt"),
    lastUsedAt: requireString(record.lastUsedAt, "lastUsedAt"),
  });
}

function decodeCanonicalBase64(value) {
  if (typeof value !== "string" || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) {
    throw new SandboxIntegrityError("Sandbox platform returned invalid base64 file content", {
      code: "INVALID_FILE_CONTENT",
    });
  }
  const bytes = Buffer.from(value, "base64");
  if (bytes.toString("base64") !== value) {
    throw new SandboxIntegrityError("Sandbox platform returned non-canonical base64 file content", {
      code: "INVALID_FILE_CONTENT",
    });
  }
  return new Uint8Array(bytes);
}

function parseResponseSize(value) {
  if (value === null || !/^(0|[1-9][0-9]*)$/.test(value)) {
    throw new SandboxIntegrityError("Sandbox platform returned invalid Content-Length", {
      code: "INVALID_STREAMING_RESPONSE",
    });
  }
  const result = Number(value);
  if (!Number.isSafeInteger(result) || result > MAX_FILE_TRANSFER_BYTES) {
    throw new SandboxTransferTooLargeError("Sandbox platform returned an unsupported Content-Length", {
      code: "TRANSFER_TOO_LARGE",
    });
  }
  return result;
}

function parseContentDigest(value) {
  const match = /^sha-256=:([A-Za-z0-9+/]{43}=):$/.exec(value ?? "");
  if (match === null) {
    throw new SandboxIntegrityError("Sandbox platform returned invalid Content-Digest", {
      code: "INVALID_CONTENT_DIGEST",
    });
  }
  const bytes = Buffer.from(match[1], "base64");
  if (bytes.byteLength !== 32 || bytes.toString("base64") !== match[1]) {
    throw new SandboxIntegrityError("Sandbox platform returned invalid Content-Digest", {
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
    throw new SandboxTransferTooLargeError("File transfer exceeds the 64 MiB limit", {
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

function requireTimeout(value) {
  if (!Number.isSafeInteger(value) || value <= 0) throw new TypeError("timeoutMs must be a positive safe integer");
  return value;
}

function requireInteger(value, name) {
  if (!Number.isInteger(value)) throw new SandboxError(`Sandbox platform returned invalid ${name}`);
  return value;
}

function requireLeaseStatus(value) {
  if (value !== "active" && value !== "released" && value !== "expired") {
    throw new SandboxError("Sandbox platform returned an invalid lease status");
  }
  return value;
}

function requireString(value, name) {
  if (typeof value !== "string") throw new SandboxError(`Sandbox platform returned invalid ${name}`);
  return value;
}

function requireNonEmpty(value, name) {
  if (typeof value !== "string" || !value.trim()) throw new TypeError(`${name} must be a non-empty string`);
  return value;
}

function requireIdentity(value, name) {
  if (typeof value !== "string" || !value.trim() || value.length > 200) {
    throw new TypeError(`${name} must be a non-empty string of at most 200 characters`);
  }
  return value;
}
