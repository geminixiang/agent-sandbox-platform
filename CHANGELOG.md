# Changelog

## [Unreleased]

### Added

- Add an expiring `/v1/leases` HTTP contract and zero-runtime-dependency TypeScript SDK.
- Add short-lived HMAC Subject tokens that derive Tenant Scope from verified `(Consumer, Subject)` claims.
- Add a local process Lease backend for trusted contract development only.
- Add a Kubernetes Agent Sandbox backend with server-side Pool mapping, RuntimeClass verification, Pod exec/file transport, restart recovery, and Claim cleanup.
- Add atomic single-replica active Lease quotas per Tenant Scope, Consumer, and Pool.
- Add cross-Subject and cross-Consumer attack tests covering inspect, exec, files, release, delete, and idempotency replay.
- Add a real Colima/gVisor integration test covering acquire, files, exec, recovery, quota, release, expiry, and cleanup.

### Security

- Require every Lease, Workspace, and idempotency lookup to include Tenant Scope.
- Return identical `404 LEASE_NOT_FOUND` responses for unknown and cross-scope Lease IDs.
- Store only server-keyed Tenant, Consumer, idempotency, and Pool hashes in Kubernetes metadata.
- Reject a claimed runtime whose Pod does not use the server-configured RuntimeClass.
