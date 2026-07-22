# Agent Sandbox Platform

A shared sandbox control plane and lightweight SDK for AI agents.

## Responsibility

The Platform grants temporary Leases while hiding sandbox infrastructure and lifecycle details. Consumers such as mikan use only the HTTP SDK; they do not depend on Kubernetes clients, CRDs, Helm, Kata, gVisor, or machine provisioning.

```text
Consumer → @geminixiang/sandbox-sdk → HTTP control plane → sandbox backend
```

Mikan is the initial Primary Design Partner, not the only intended Consumer.

## Current phase

The platform provides:

1. temporary Lease rights rather than ownership of Pods, VMs, or Sandboxes,
2. `(Consumer, Subject)` isolation for every Lease and Workspace operation,
3. a Kubernetes Agent Sandbox backend with server-side Pool mapping, runtime verification, restart recovery, and release/expiry cleanup,
4. atomic single-replica quotas per Tenant Scope, Consumer, and Pool.

The local process backend remains available for trusted contract development only. **It does not isolate untrusted code.**

## Packages

- `@geminixiang/sandbox-contracts`: `/v1` protocol constants and types
- `@geminixiang/sandbox-sdk`: 3 KiB, zero-runtime-dependency HTTP SDK
- `@geminixiang/sandbox-control-plane`: private HTTP server

## Quick start

Start the control plane with server-side Consumer secrets:

```bash
npm install
SANDBOX_CONSUMER_SECRETS='{"mikan-dev":"dev-secret"}' npm start
```

Use the SDK from another process:

```js
import { SandboxPlatformClient } from "@geminixiang/sandbox-sdk";

const client = new SandboxPlatformClient({
  baseUrl: "http://127.0.0.1:8787",
  consumerId: "mikan-dev",
  subjectId: "opaque-user-id",
  consumerSecret: "dev-secret",
});

const { lease } = await client.acquire(
  { pool: "local", ttlSeconds: 900 },
  { idempotencyKey: crypto.randomUUID() },
);
await lease.exec("printf hello");
await lease.release();
```

## Kubernetes backend

See [`docs/kubernetes-backend.md`](docs/kubernetes-backend.md) for requirements, configuration, lifecycle, and the Colima integration test.

The current quota lock is process-local; Kubernetes mode must run as a single control-plane replica until distributed acquisition locking is implemented.

## Verify

```bash
npm test
npm run test:package
```

The test suite includes cross-Subject and cross-Consumer access attempts for inspect, exec, files, release, delete, and idempotency replay.

## Next milestone

Review the single-cluster implementation under real mikan workloads before choosing the next capability. Billing, Channels, Restore, and multi-cluster placement remain intentionally out of scope.
