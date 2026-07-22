# Architecture

## External seam

Consumers learn one interface: authenticated `/v1/leases` operations for acquire, inspect, execute, file I/O, release, and delete. TypeScript and Python SDKs are thin adapters over that language-neutral interface. SDKs never expose Kubernetes or cloud-provider infrastructure concepts.

Consumers hold temporary Lease rights. They never own or address the underlying Pod, VM, container, or Sandbox.

## Tenant isolation

The control plane verifies a short-lived Subject token and derives Tenant Scope `(Consumer, Subject)`. The scope is passed into every backend operation and every lookup is performed as `(scope, leaseId)`; possession of a Lease ID is never authorization.

Cross-scope and missing resources produce the same status, code, and message. Idempotency mappings include the Tenant Scope.

## Backend seam

The production control plane is implemented in Go. A backend supplies:

- `acquire(scope, { pool, ttlSeconds, idempotencyKey })`
- `get(scope, leaseId)`
- `exec(scope, leaseId, request, signal)`
- `readFile(scope, leaseId, request)`
- `writeFile(scope, leaseId, request)`
- `release(scope, leaseId)`
- `delete(scope, leaseId)`
- `close()`

The Kubernetes backend translates this interface into Agent Sandbox claims, WarmPools, Pod exec operations, persistent workspaces, runtime verification, restart recovery, and release/expiry cleanup. It is the only production adapter. Unit and contract tests use test-only fakes rather than a host-process implementation.

## Deployment portability

The Kubernetes distribution targets GKE, Amazon EKS, Azure AKS, macOS with Colima and k3s, and Linux Kubernetes with k3s as the initial local reference. Helm values and internal adapters absorb differences in cloud identity, storage classes, ingress, load balancers, and runtime classes. These differences do not enter the Consumer interface or SDKs.

## Quota consistency

Acquisition is serialized inside one control-plane process across idempotency lookup, active Lease counting, quota checks, and Claim creation. This provides atomic limits per Tenant Scope, Consumer, and Pool for a single replica. Multi-replica deployment is unsupported until the same critical section is backed by a distributed lock.

## Invariants

- A Lease is a temporary right and becomes unusable after release or expiry.
- Every Lease, Workspace, and idempotency mapping belongs to exactly one Tenant Scope.
- Runtime paths stay beneath `/workspace`.
- Backends, not Consumers, own readiness, retries, and infrastructure replacement.
- SDK releases have zero runtime dependencies.
- Kubernetes resource metadata contains only server-keyed hashes, never raw Consumer or Subject identities.
- Claims being deleted are immediately excluded from lookup, recovery, idempotency replay, and quota accounting.
