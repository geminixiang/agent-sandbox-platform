# Architecture

## External seam

Consumers learn one interface: authenticated `/v1` HTTP operations for acquire, inspect, execute, file I/O, release, and delete. The TypeScript SDK is a convenience adapter over that interface.

## Backend seam

The control plane owns backend selection and lifecycle. A backend supplies:

- `acquire({ key, pool })`
- `get(id)`
- `exec(id, request, signal)`
- `readFile(id, request)`
- `writeFile(id, request)`
- `release(id)`
- `delete(id)`
- `close()`

The process backend is development-only. The Kubernetes backend will translate this interface into SandboxClaim, WarmPool, router, PVC, runtime verification, and recovery operations.

## Invariants

- Acquisition is idempotent by `(pool, key)` while a sandbox is ready.
- Runtime paths stay beneath `/workspace`.
- Release ends active use but preserves a record until deletion.
- Delete is permanent.
- Backends, not consumers, own retries, readiness, and recovery.
- SDK releases have zero runtime dependencies.
