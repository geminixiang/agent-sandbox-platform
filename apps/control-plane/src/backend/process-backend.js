import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";

const RUNTIME_WORKSPACE = "/workspace";

export class ProcessSandboxBackend {
  constructor(options = {}) {
    this.rootDir = options.rootDir;
    this.sandboxes = new Map();
    this.activeByKey = new Map();
  }

  async acquire({ key, pool }) {
    const activeId = this.activeByKey.get(`${pool}\0${key}`);
    const active = activeId && this.sandboxes.get(activeId);
    if (active?.record.status === "ready") {
      active.record.lastUsedAt = new Date().toISOString();
      return { sandbox: structuredClone(active.record), reused: true };
    }

    const workspaceDir = await mkdtemp(join(this.rootDir ?? tmpdir(), "sandbox-platform-"));
    const timestamp = new Date().toISOString();
    const record = {
      id: `sbx_${randomUUID().replaceAll("-", "")}`,
      key,
      pool,
      status: "ready",
      createdAt: timestamp,
      lastUsedAt: timestamp,
    };
    this.sandboxes.set(record.id, { record, workspaceDir });
    this.activeByKey.set(`${pool}\0${key}`, record.id);
    return { sandbox: structuredClone(record), reused: false };
  }

  get(id) {
    return structuredClone(this.requireSandbox(id).record);
  }

  async exec(id, request, signal) {
    const sandbox = this.requireReadySandbox(id);
    const cwd = this.resolveRuntimePath(sandbox, request.cwd ?? RUNTIME_WORKSPACE);
    await mkdir(cwd, { recursive: true });
    sandbox.record.lastUsedAt = new Date().toISOString();
    return executeShell(request.command, {
      cwd,
      env: { ...process.env, ...request.env },
      timeoutMs: request.timeoutSeconds ? request.timeoutSeconds * 1000 : undefined,
      signal,
    });
  }

  async readFile(id, request) {
    const sandbox = this.requireReadySandbox(id);
    const path = this.resolveRuntimePath(sandbox, request.path);
    const encoding = request.encoding ?? "utf8";
    const content = await readFile(path, encoding === "base64" ? null : "utf8");
    sandbox.record.lastUsedAt = new Date().toISOString();
    return {
      path: request.path,
      content: encoding === "base64" ? content.toString("base64") : content,
      encoding,
    };
  }

  async writeFile(id, request) {
    const sandbox = this.requireReadySandbox(id);
    const path = this.resolveRuntimePath(sandbox, request.path);
    await mkdir(dirname(path), { recursive: true });
    const content =
      request.encoding === "base64" ? Buffer.from(request.content, "base64") : request.content;
    await writeFile(path, content);
    sandbox.record.lastUsedAt = new Date().toISOString();
    return { path: request.path };
  }

  async release(id) {
    const sandbox = this.requireSandbox(id);
    sandbox.record.status = "released";
    sandbox.record.lastUsedAt = new Date().toISOString();
    this.activeByKey.delete(`${sandbox.record.pool}\0${sandbox.record.key}`);
    return structuredClone(sandbox.record);
  }

  async delete(id) {
    const sandbox = this.requireSandbox(id);
    this.activeByKey.delete(`${sandbox.record.pool}\0${sandbox.record.key}`);
    this.sandboxes.delete(id);
    await rm(sandbox.workspaceDir, { recursive: true, force: true });
  }

  async close() {
    await Promise.all(Array.from(this.sandboxes.keys(), (id) => this.delete(id)));
  }

  requireSandbox(id) {
    const sandbox = this.sandboxes.get(id);
    if (!sandbox) throw backendError("NOT_FOUND", `Sandbox '${id}' does not exist`, 404);
    return sandbox;
  }

  requireReadySandbox(id) {
    const sandbox = this.requireSandbox(id);
    if (sandbox.record.status !== "ready") {
      throw backendError("NOT_READY", `Sandbox '${id}' is released`, 409);
    }
    return sandbox;
  }

  resolveRuntimePath(sandbox, runtimePath) {
    const suffix = isAbsolute(runtimePath)
      ? relative(RUNTIME_WORKSPACE, runtimePath)
      : runtimePath;
    const resolved = resolve(sandbox.workspaceDir, suffix);
    const relativePath = relative(sandbox.workspaceDir, resolved);
    if (relativePath.startsWith("..") || isAbsolute(relativePath)) {
      throw backendError("INVALID_PATH", "Path must stay inside /workspace", 400);
    }
    return resolved;
  }
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
