import { randomUUID } from "node:crypto";
import { Readable, Writable } from "node:stream";
import { CoreV1Api, CustomObjectsApi, Exec, KubeConfig } from "@kubernetes/client-node";
import { createMetadataIdentity } from "./metadata-identity.js";
import { normalizeWorkspacePath } from "./workspace-path.js";

const GROUP = "extensions.agents.x-k8s.io";
const VERSION = "v1beta1";
const PLURAL = "sandboxclaims";
const SANDBOX_GROUP = "agents.x-k8s.io";
const SANDBOX_PLURAL = "sandboxes";
const MANAGED_LABEL = "sandbox.geminixiang.dev/managed";
const SCOPE_LABEL = "sandbox.geminixiang.dev/scope";
const CONSUMER_LABEL = "sandbox.geminixiang.dev/consumer";
const IDEMPOTENCY_LABEL = "sandbox.geminixiang.dev/idempotency";
const POOL_HASH_LABEL = "sandbox.geminixiang.dev/pool";
const POOL_ANNOTATION = "sandbox.geminixiang.dev/pool-name";
const CREATED_ANNOTATION = "sandbox.geminixiang.dev/created-at";
const EXPIRES_ANNOTATION = "sandbox.geminixiang.dev/expires-at";
const DEFAULT_WORKSPACE = "/workspace";

export class KubernetesLeaseBackend {
  constructor(options) {
    this.namespace = requireNonEmpty(options.namespace, "namespace");
    this.pools = validatePools(options.pools);
    this.now = options.now ?? Date.now;
    this.pollIntervalMs = options.pollIntervalMs ?? 500;
    this.readyTimeoutMs = options.readyTimeoutMs ?? 120_000;
    this.defaultTtlSeconds = options.defaultTtlSeconds ?? 900;
    this.maxTtlSeconds = options.maxTtlSeconds ?? 3600;
    this.identity = createMetadataIdentity(options.metadataSecret);

    const clients = options.clients ?? createKubernetesClients(options.kubeContext);
    this.customObjectsApi = clients.customObjectsApi;
    this.coreApi = clients.coreApi;
    this.execClient = clients.execClient;
  }

  scopeHash(scope) {
    return this.identity.scopeHash(scope);
  }

  consumerHash(scope) {
    return this.identity.consumerHash(scope);
  }

  async findByIdempotencyKey(scope, idempotencyKey) {
    const claims = await this.listClaims([
      `${MANAGED_LABEL}=true`,
      `${SCOPE_LABEL}=${this.scopeHash(scope)}`,
      `${IDEMPOTENCY_LABEL}=${this.identity.idempotencyHash(scope, idempotencyKey)}`,
    ]);
    const claim = claims.find((item) => !item.metadata?.deletionTimestamp);
    if (!claim) return undefined;
    const record = recordFromClaim(claim);
    if (Date.parse(record.expiresAt) <= this.now()) {
      await this.deleteClaim(record.id);
      return undefined;
    }
    return record;
  }

  async listActiveLeases() {
    const claims = await this.listClaims([`${MANAGED_LABEL}=true`]);
    const now = this.now();
    return claims
      .filter((claim) => !claim.metadata?.deletionTimestamp)
      .map((claim) => ({ claim, record: recordFromClaim(claim) }))
      .filter(({ record }) => record.status === "active" && Date.parse(record.expiresAt) > now)
      .map(({ claim, record }) => ({
        record,
        scopeHash: claim.metadata.labels[SCOPE_LABEL],
        consumerHash: claim.metadata.labels[CONSUMER_LABEL],
      }));
  }

  async acquire(scope, request) {
    const pool = this.requirePool(request.pool);
    const ttlSeconds = request.ttlSeconds ?? this.defaultTtlSeconds;
    validateTtl(ttlSeconds, this.maxTtlSeconds);

    const replay = await this.findByIdempotencyKey(scope, request.idempotencyKey);
    if (replay) return { lease: replay, replayed: true };

    const now = this.now();
    const leaseId = `lease-${randomUUID().replaceAll("-", "")}`;
    const claim = buildClaim({
      leaseId,
      namespace: this.namespace,
      poolName: request.pool,
      warmPoolName: pool.warmPoolName,
      poolHash: hashPool(this.identity, request.pool),
      scopeHash: this.scopeHash(scope),
      consumerHash: this.consumerHash(scope),
      idempotencyHash: this.identity.idempotencyHash(scope, request.idempotencyKey),
      createdAt: new Date(now).toISOString(),
      expiresAt: new Date(now + ttlSeconds * 1000).toISOString(),
    });

    await this.customObjectsApi.createNamespacedCustomObject({
      group: GROUP,
      version: VERSION,
      namespace: this.namespace,
      plural: PLURAL,
      body: claim,
    });
    try {
      const ready = await this.waitForReadyClaim(leaseId);
      await this.verifyRuntime(ready, pool);
      return { lease: recordFromClaim(ready), replayed: false };
    } catch (error) {
      await this.deleteClaim(leaseId).catch(() => undefined);
      throw error;
    }
  }

  async get(scope, id) {
    const claim = await this.requireClaim(scope, id);
    const record = recordFromClaim(claim);
    if (Date.parse(record.expiresAt) <= this.now()) {
      await this.deleteClaim(id);
      return { ...record, status: "expired" };
    }
    return record;
  }

  async exec(scope, id, request, signal) {
    const lease = await this.requireActiveLease(scope, id);
    const pod = await this.resolvePod(lease.claim, this.requirePool(lease.record.pool));
    const cwd = normalizeWorkspacePath(request.cwd ?? DEFAULT_WORKSPACE);
    const env = Object.entries(request.env ?? {}).map(([name, value]) => {
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || value.includes("\0")) {
        throw backendError("INVALID_ENV", `Invalid environment variable '${name}'`, 400);
      }
      return `${name}=${value}`;
    });
    const command = [
      "/bin/sh",
      "-c",
      'cd "$1" && shift && exec env "$@"',
      "platform",
      cwd,
      ...env,
      "/bin/sh",
      "-lc",
      request.command,
    ];
    return this.runPodCommand(pod, command, {
      signal,
      timeoutMs: request.timeoutSeconds ? request.timeoutSeconds * 1000 : undefined,
    });
  }

  async readFile(scope, id, request) {
    const lease = await this.requireActiveLease(scope, id);
    const pod = await this.resolvePod(lease.claim, this.requirePool(lease.record.pool));
    const path = normalizeWorkspacePath(request.path);
    const result = await this.runPodCommand(pod, [
      "/bin/sh",
      "-c",
      'base64 "$1" | tr -d "\\n"',
      "platform",
      path,
    ]);
    if (result.code !== 0) throw backendError("FILE_READ_FAILED", result.stderr || "File read failed", 400);
    const content = request.encoding === "base64"
      ? result.stdout
      : Buffer.from(result.stdout, "base64").toString("utf8");
    return { path: request.path, content, encoding: request.encoding ?? "utf8" };
  }

  async writeFile(scope, id, request) {
    const lease = await this.requireActiveLease(scope, id);
    const pod = await this.resolvePod(lease.claim, this.requirePool(lease.record.pool));
    const path = normalizeWorkspacePath(request.path);
    const source = request.encoding === "base64"
      ? request.content
      : Buffer.from(request.content, "utf8").toString("base64");
    const result = await this.runPodCommand(
      pod,
      [
        "/bin/sh",
        "-c",
        'mkdir -p "$(dirname "$1")" && base64 -d > "$1"',
        "platform",
        path,
      ],
      { stdin: source },
    );
    if (result.code !== 0) throw backendError("FILE_WRITE_FAILED", result.stderr || "File write failed", 400);
    return { path: request.path };
  }

  async release(scope, id) {
    const claim = await this.requireClaim(scope, id);
    const record = { ...recordFromClaim(claim), status: "released", lastUsedAt: new Date(this.now()).toISOString() };
    await this.deleteClaim(id);
    return record;
  }

  async delete(scope, id) {
    await this.requireClaim(scope, id);
    await this.deleteClaim(id);
  }

  async recover() {
    await this.sweepExpired();
    return this.listActiveLeases();
  }

  async sweepExpired() {
    const claims = await this.listClaims([`${MANAGED_LABEL}=true`]);
    const now = this.now();
    const expired = claims.filter(
      (claim) =>
        !claim.metadata?.deletionTimestamp &&
        Date.parse(claim.metadata.annotations[EXPIRES_ANNOTATION]) <= now,
    );
    await Promise.all(expired.map((claim) => this.deleteClaim(claim.metadata.name)));
    return expired.length;
  }

  async close() {}

  requirePool(poolName) {
    const pool = this.pools[poolName];
    if (!pool) throw backendError("POOL_NOT_FOUND", `Pool '${poolName}' is not configured`, 404);
    return pool;
  }

  async requireClaim(scope, id) {
    let claim;
    try {
      claim = await this.customObjectsApi.getNamespacedCustomObject({
        group: GROUP,
        version: VERSION,
        namespace: this.namespace,
        plural: PLURAL,
        name: id,
      });
    } catch (error) {
      if (isNotFound(error)) throw leaseNotFound();
      throw error;
    }
    if (
      claim.metadata?.deletionTimestamp ||
      claim.metadata?.labels?.[MANAGED_LABEL] !== "true" ||
      claim.metadata?.labels?.[SCOPE_LABEL] !== this.scopeHash(scope)
    ) {
      throw leaseNotFound();
    }
    return claim;
  }

  async requireActiveLease(scope, id) {
    const claim = await this.requireClaim(scope, id);
    const record = recordFromClaim(claim);
    if (Date.parse(record.expiresAt) <= this.now()) {
      await this.deleteClaim(id);
      throw backendError("LEASE_NOT_ACTIVE", "Lease is not active", 409);
    }
    return { claim, record };
  }

  async listClaims(selectors) {
    const response = await this.customObjectsApi.listNamespacedCustomObject({
      group: GROUP,
      version: VERSION,
      namespace: this.namespace,
      plural: PLURAL,
      labelSelector: selectors.join(","),
    });
    return response.items ?? [];
  }

  async waitForReadyClaim(name) {
    const deadline = this.now() + this.readyTimeoutMs;
    while (this.now() < deadline) {
      const claim = await this.customObjectsApi.getNamespacedCustomObject({
        group: GROUP,
        version: VERSION,
        namespace: this.namespace,
        plural: PLURAL,
        name,
      });
      const sandboxName = claim.status?.sandbox?.name;
      if (sandboxName) {
        const sandbox = await this.customObjectsApi.getNamespacedCustomObject({
          group: SANDBOX_GROUP,
          version: VERSION,
          namespace: this.namespace,
          plural: SANDBOX_PLURAL,
          name: sandboxName,
        });
        const ready = sandbox.status?.conditions?.find(
          (condition) => condition.type === "Ready" && condition.status === "True",
        );
        if (ready) return claim;
      }
      await delay(this.pollIntervalMs);
    }
    throw backendError("LEASE_READY_TIMEOUT", `Lease '${name}' did not become ready`, 504);
  }

  async resolvePod(claim, pool) {
    const sandboxName = claim.status?.sandbox?.name ?? claim.metadata.name;
    const sandbox = await this.customObjectsApi.getNamespacedCustomObject({
      group: SANDBOX_GROUP,
      version: VERSION,
      namespace: this.namespace,
      plural: SANDBOX_PLURAL,
      name: sandboxName,
    });
    const podName = sandbox.metadata?.annotations?.["agents.x-k8s.io/pod-name"] ?? sandboxName;
    const pod = await this.coreApi.readNamespacedPod({ name: podName, namespace: this.namespace });
    const container = pool.containerName ?? pod.spec?.containers?.[0]?.name;
    if (!container) throw backendError("CONTAINER_NOT_FOUND", "Lease pod has no executable container", 500);
    return { name: podName, container };
  }

  async verifyRuntime(claim, pool) {
    const pod = await this.resolvePod(claim, pool);
    const resource = await this.coreApi.readNamespacedPod({ name: pod.name, namespace: this.namespace });
    const actual = resource.spec?.runtimeClassName;
    if (actual !== pool.runtimeClassName) {
      throw backendError(
        "RUNTIME_CLASS_MISMATCH",
        `Lease pod uses RuntimeClass '${actual ?? "none"}', expected '${pool.runtimeClassName}'`,
        500,
      );
    }
  }

  async runPodCommand(pod, command, options = {}) {
    if (options.signal?.aborted) {
      throw backendError("ABORTED", "Command was aborted or timed out", 408);
    }
    const stdout = createOutputCapture();
    const stderr = createOutputCapture();
    const stdin = options.stdin === undefined ? null : Readable.from([options.stdin]);
    let status;
    const socket = await this.execClient.exec(
      this.namespace,
      pod.name,
      pod.container,
      command,
      stdout,
      stderr,
      stdin,
      false,
      (value) => { status = value; },
    );
    await waitForSocket(socket, options);
    if (stdout.exceeded || stderr.exceeded) {
      throw backendError("OUTPUT_TOO_LARGE", "Command output exceeded 10 MiB", 413);
    }
    return { stdout: stdout.text(), stderr: stderr.text(), code: exitCode(status) };
  }

  async deleteClaim(name) {
    try {
      await this.customObjectsApi.deleteNamespacedCustomObject({
        group: GROUP,
        version: VERSION,
        namespace: this.namespace,
        plural: PLURAL,
        name,
        propagationPolicy: "Foreground",
      });
    } catch (error) {
      if (!isNotFound(error)) throw error;
    }
  }
}

export function createKubernetesClients(kubeContext) {
  const kubeConfig = new KubeConfig();
  kubeConfig.loadFromDefault();
  if (kubeContext) kubeConfig.setCurrentContext(kubeContext);
  return {
    customObjectsApi: kubeConfig.makeApiClient(CustomObjectsApi),
    coreApi: kubeConfig.makeApiClient(CoreV1Api),
    execClient: new Exec(kubeConfig),
  };
}

function validatePools(pools) {
  if (!pools || typeof pools !== "object" || Array.isArray(pools)) {
    throw new TypeError("pools must be an object");
  }
  const validated = {};
  for (const [name, pool] of Object.entries(pools)) {
    requireNonEmpty(name, "pool name");
    if (!pool || typeof pool !== "object" || Array.isArray(pool)) {
      throw new TypeError(`Pool '${name}' must be an object`);
    }
    validated[name] = {
      warmPoolName: requireNonEmpty(pool.warmPoolName, `Pool '${name}' warmPoolName`),
      runtimeClassName: requireNonEmpty(
        pool.runtimeClassName,
        `Pool '${name}' runtimeClassName`,
      ),
      ...(pool.containerName
        ? { containerName: requireNonEmpty(pool.containerName, `Pool '${name}' containerName`) }
        : {}),
    };
  }
  if (Object.keys(validated).length === 0) throw new TypeError("At least one pool is required");
  return validated;
}

function requireNonEmpty(value, name) {
  if (typeof value !== "string" || !value.trim()) throw new TypeError(`${name} is required`);
  return value;
}

function buildClaim(input) {
  return {
    apiVersion: `${GROUP}/${VERSION}`,
    kind: "SandboxClaim",
    metadata: {
      name: input.leaseId,
      namespace: input.namespace,
      labels: {
        [MANAGED_LABEL]: "true",
        [SCOPE_LABEL]: input.scopeHash,
        [CONSUMER_LABEL]: input.consumerHash,
        [IDEMPOTENCY_LABEL]: input.idempotencyHash,
        [POOL_HASH_LABEL]: input.poolHash,
      },
      annotations: {
        [POOL_ANNOTATION]: input.poolName,
        [CREATED_ANNOTATION]: input.createdAt,
        [EXPIRES_ANNOTATION]: input.expiresAt,
      },
    },
    spec: {
      warmPoolRef: { name: input.warmPoolName },
      lifecycle: { shutdownPolicy: "DeleteForeground", shutdownTime: input.expiresAt },
    },
  };
}

function recordFromClaim(claim) {
  const annotations = claim.metadata?.annotations ?? {};
  return {
    id: claim.metadata.name,
    pool: annotations[POOL_ANNOTATION],
    status: "active",
    createdAt: annotations[CREATED_ANNOTATION] ?? claim.metadata.creationTimestamp,
    expiresAt: annotations[EXPIRES_ANNOTATION],
    lastUsedAt: annotations[CREATED_ANNOTATION] ?? claim.metadata.creationTimestamp,
  };
}

function hashPool(identity, pool) {
  return identity.idempotencyHash({ consumerId: "pool", subjectId: "pool" }, pool);
}

function validateTtl(value, max) {
  if (!Number.isInteger(value) || value <= 0 || value > max) {
    throw backendError("INVALID_LEASE_TTL", `ttlSeconds must be an integer between 1 and ${max}`, 400);
  }
}

function createOutputCapture(limit = 10 * 1024 * 1024) {
  const chunks = [];
  let size = 0;
  const stream = new Writable({
    write(chunk, _encoding, callback) {
      size += chunk.length;
      if (size <= limit) chunks.push(Buffer.from(chunk));
      callback();
    },
  });
  Object.defineProperties(stream, {
    exceeded: { get: () => size > limit },
    text: { value: () => Buffer.concat(chunks).toString("utf8") },
  });
  return stream;
}

function waitForSocket(socket, options) {
  return new Promise((resolve, reject) => {
    let timeout;
    const abort = () => {
      socket.close();
      reject(backendError("ABORTED", "Command was aborted or timed out", 408));
    };
    socket.once("close", resolve);
    socket.once("error", reject);
    if (options.timeoutMs) timeout = setTimeout(abort, options.timeoutMs);
    options.signal?.addEventListener("abort", abort, { once: true });
    socket.once("close", () => {
      if (timeout) clearTimeout(timeout);
      options.signal?.removeEventListener("abort", abort);
    });
  });
}

function exitCode(status) {
  if (!status || status.status === "Success") return 0;
  const cause = status.details?.causes?.find((item) => item.reason === "ExitCode");
  return Number(cause?.message ?? 1);
}

function isNotFound(error) {
  return error?.statusCode === 404 || error?.response?.statusCode === 404 || error?.body?.code === 404;
}

function leaseNotFound() {
  return backendError("LEASE_NOT_FOUND", "Lease not found", 404);
}

function backendError(code, message, status) {
  return Object.assign(new Error(message), { code, status });
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
