# Architecture

## External seam

Consumers learn one interface: authenticated `/v1/leases` operations for acquire, inspect, execute, file I/O, release, and delete. The TypeScript SDK is a zero-runtime-dependency adapter over that interface.

Consumers hold temporary Lease rights. They never own or address the underlying Pod, VM, container, or Sandbox.

## Tenant isolation

The control plane verifies a short-lived Subject token and derives Tenant Scope `(Consumer, Subject)`. The scope is passed into every backend operation and every lookup is performed as `(scope, leaseId)`; possession of a Lease ID is never authorization.

Cross-scope and missing resources produce the same status, code, and message. Idempotency mappings include the Tenant Scope.

## Backend seam

A backend supplies:

- `acquire(scope, { pool, ttlSeconds, idempotencyKey })`
- `get(scope, leaseId)`
- `exec(scope, leaseId, request, signal)`
- `readFile(scope, leaseId, request)`
- `writeFile(scope, leaseId, request)`
- `release(scope, leaseId)`
- `delete(scope, leaseId)`
- `close()`

The process backend is development-only. A Kubernetes backend will translate the same interface into Agent Sandbox claims, warm pools, router operations, persistent workspaces, runtime verification, and recovery.

## Invariants

- A Lease is a temporary right and becomes unusable after release or expiry.
- Every Lease, Workspace, and idempotency mapping belongs to exactly one Tenant Scope.
- Runtime paths stay beneath `/workspace`.
- Backends, not Consumers, own readiness, retries, and infrastructure replacement.
- SDK releases have zero runtime dependencies.
