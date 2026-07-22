import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";
import { createMetadataIdentity } from "./metadata-identity.js";

const RUNTIME_WORKSPACE = "/workspace";

export class ProcessLeaseBackend {
  constructor(options = {}) {
    this.rootDir = options.rootDir;
    this.now = options.now ?? Date.now;
    this.defaultTtlSeconds = options.defaultTtlSeconds ?? 900;
    this.maxTtlSeconds = options.maxTtlSeconds ?? 3600;
    this.identity = createMetadataIdentity(options.metadataSecret ?? "process-development-only");
    this.leases = new Map();
    this.leaseByIdempotencyKey = new Map();
  }

  async acquire(scope, { pool, idempotencyKey, ttlSeconds = this.defaultTtlSeconds }) {
    if (!Number.isInteger(ttlSeconds) || ttlSeconds <= 0 || ttlSeconds > this.maxTtlSeconds) {
      throw backendError(
        "INVALID_LEASE_TTL",
        `ttlSeconds must be an integer between 1 and ${this.maxTtlSeconds}`,
        400,
      );
    }
    const mappingKey = scopeKey(scope, idempotencyKey);
    const existingId = this.leaseByIdempotencyKey.get(mappingKey);
    const existing = existingId && this.leases.get(existingId);
    if (existing) {
      this.updateExpiration(existing);
      if (existing.record.status === "expired") {
        await this.deleteLease(existingId, existing);
      } else {
        return { lease: publicRecord(existing.record), replayed: true };
      }
    }

    const workspaceDir = await mkdtemp(join(this.rootDir ?? tmpdir(), "sandbox-platform-"));
    const now = this.now();
    const timestamp = new Date(now).toISOString();
    const record = {
      id: `lease_${randomUUID().replaceAll("-", "")}`,
      pool,
      status: "active",
      createdAt: timestamp,
      expiresAt: new Date(now + ttlSeconds * 1000).toISOString(),
      lastUsedAt: timestamp,
    };
    const scopeHash = this.scopeHash(scope);
    const consumerHash = this.consumerHash(scope);
    this.leases.set(record.id, {
      record,
      scope: { ...scope },
      scopeHash,
      consumerHash,
      workspaceDir,
      mappingKey,
    });
    this.leaseByIdempotencyKey.set(mappingKey, record.id);
    return { lease: publicRecord(record), replayed: false };
  }

  async findByIdempotencyKey(scope, idempotencyKey) {
    const leaseId = this.leaseByIdempotencyKey.get(scopeKey(scope, idempotencyKey));
    const lease = leaseId && this.leases.get(leaseId);
    if (!lease) return undefined;
    this.updateExpiration(lease);
    if (lease.record.status === "expired") {
      await this.deleteLease(leaseId, lease);
      return undefined;
    }
    return publicRecord(lease.record);
  }

  listActiveLeases() {
    const active = [];
    for (const lease of this.leases.values()) {
      this.updateExpiration(lease);
      if (lease.record.status === "active") {
        active.push({
          record: publicRecord(lease.record),
          scopeHash: lease.scopeHash,
          consumerHash: lease.consumerHash,
        });
      }
    }
    return active;
  }

  scopeHash(scope) {
    return this.identity.scopeHash(scope);
  }

  consumerHash(scope) {
    return this.identity.consumerHash(scope);
  }

  get(scope, id) {
    return publicRecord(this.requireLease(scope, id).record);
  }

  async exec(scope, id, request, signal) {
    const lease = this.requireActiveLease(scope, id);
    const cwd = this.resolveRuntimePath(lease, request.cwd ?? RUNTIME_WORKSPACE);
    await mkdir(cwd, { recursive: true });
    this.touch(lease);
    return executeShell(request.command, {
      cwd,
      env: { ...process.env, ...request.env },
      timeoutMs: request.timeoutSeconds ? request.timeoutSeconds * 1000 : undefined,
      signal,
    });
  }

  async readFile(scope, id, request) {
    const lease = this.requireActiveLease(scope, id);
    const path = this.resolveRuntimePath(lease, request.path);
    const encoding = request.encoding ?? "utf8";
    const content = await readFile(path, encoding === "base64" ? null : "utf8");
    this.touch(lease);
    return {
      path: request.path,
      content: encoding === "base64" ? content.toString("base64") : content,
      encoding,
    };
  }

  async writeFile(scope, id, request) {
    const lease = this.requireActiveLease(scope, id);
    const path = this.resolveRuntimePath(lease, request.path);
    await mkdir(dirname(path), { recursive: true });
    const content =
      request.encoding === "base64" ? Buffer.from(request.content, "base64") : request.content;
    await writeFile(path, content);
    this.touch(lease);
    return { path: request.path };
  }

  async release(scope, id) {
    const lease = this.requireActiveLease(scope, id);
    lease.record.status = "released";
    this.touch(lease);
    if (this.leaseByIdempotencyKey.get(lease.mappingKey) === id) {
      this.leaseByIdempotencyKey.delete(lease.mappingKey);
    }
    return publicRecord(lease.record);
  }

  async delete(scope, id) {
    const lease = this.requireLease(scope, id);
    await this.deleteLease(id, lease);
  }

  async close() {
    await Promise.all(Array.from(this.leases, ([id, lease]) => this.deleteLease(id, lease)));
  }

  requireLease(scope, id) {
    const lease = this.leases.get(id);
    if (!lease || !sameScope(scope, lease.scope)) throw leaseNotFound();
    this.updateExpiration(lease);
    return lease;
  }

  requireActiveLease(scope, id) {
    const lease = this.requireLease(scope, id);
    if (lease.record.status !== "active") {
      throw backendError("LEASE_NOT_ACTIVE", "Lease is not active", 409);
    }
    return lease;
  }

  updateExpiration(lease) {
    if (lease.record.status === "active" && this.now() >= Date.parse(lease.record.expiresAt)) {
      lease.record.status = "expired";
    }
  }

  touch(lease) {
    lease.record.lastUsedAt = new Date(this.now()).toISOString();
  }

  async deleteLease(id, lease) {
    this.leases.delete(id);
    if (this.leaseByIdempotencyKey.get(lease.mappingKey) === id) {
      this.leaseByIdempotencyKey.delete(lease.mappingKey);
    }
    await rm(lease.workspaceDir, { recursive: true, force: true });
  }

  resolveRuntimePath(lease, runtimePath) {
    const suffix = isAbsolute(runtimePath)
      ? relative(RUNTIME_WORKSPACE, runtimePath)
      : runtimePath;
    const resolved = resolve(lease.workspaceDir, suffix);
    const relativePath = relative(lease.workspaceDir, resolved);
    if (relativePath.startsWith("..") || isAbsolute(relativePath)) {
      throw backendError("INVALID_PATH", "Path must stay inside /workspace", 400);
    }
    return resolved;
  }
}

function sameScope(left, right) {
  return left.consumerId === right.consumerId && left.subjectId === right.subjectId;
}

function scopeKey(scope, idempotencyKey) {
  return JSON.stringify([scope.consumerId, scope.subjectId, idempotencyKey]);
}

function publicRecord(record) {
  return structuredClone(record);
}

function leaseNotFound() {
  return backendError("LEASE_NOT_FOUND", "Lease not found", 404);
}

function executeShell(command, options) {
  return new Promise((resolvePromise, reject) => {
    execFile(
      "/bin/sh",
      ["-lc", command],
      {
        cwd: options.cwd,
        env: options.env,
        timeout: options.timeoutMs,
        signal: options.signal,
        maxBuffer: 10 * 1024 * 1024,
      },
      (error, stdout, stderr) => {
        if (!error) {
          resolvePromise({ stdout, stderr, code: 0 });
          return;
        }
        if (error.killed || error.name === "AbortError") {
          reject(backendError("ABORTED", "Command was aborted or timed out", 408));
          return;
        }
        resolvePromise({ stdout, stderr, code: error.code ?? 1 });
      },
    );
  });
}

function backendError(code, message, status) {
  return Object.assign(new Error(message), { code, status });
}
