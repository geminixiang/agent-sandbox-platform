# Agent Sandbox Platform

A shared sandbox control plane and lightweight SDK for AI agents.

## Responsibility

The Platform grants temporary Leases while hiding sandbox infrastructure and lifecycle details. Consumers such as mikan use only the HTTP SDK; they do not depend on Kubernetes clients, CRDs, Helm, Kata, gVisor, or machine provisioning.

```text
Consumer → @geminixiang/sandbox-sdk → HTTP control plane → sandbox backend
```

Mikan is the initial Primary Design Partner, not the only intended Consumer.

## Current phase

Phase 0 proves the two core invariants:

1. Consumers receive temporary Leases, not ownership of Pods, VMs, or Sandboxes.
2. Every Lease and Workspace operation is isolated by `(Consumer, Subject)`, even when a Consumer routing bug supplies another Subject's Lease ID.

The local process backend is for development and contract tests only. **It does not isolate untrusted code.** Kubernetes Agent Sandbox support will be added behind the same backend interface later.

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

## Verify

```bash
npm test
npm run test:package
```

The test suite includes cross-Subject and cross-Consumer access attempts for inspect, exec, files, release, delete, and idempotency replay.

## Next milestone

Add a Kubernetes Agent Sandbox backend inside the control plane without changing the Lease or Tenant Scope interface.
