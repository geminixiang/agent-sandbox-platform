# Changelog

## [Unreleased]

### Added

- Add an expiring `/v1/leases` HTTP contract and zero-runtime-dependency TypeScript SDK.
- Add short-lived HMAC Subject tokens that derive Tenant Scope from verified `(Consumer, Subject)` claims.
- Add a local process Lease backend for trusted contract development only.
- Add cross-Subject and cross-Consumer attack tests covering inspect, exec, files, release, delete, and idempotency replay.

### Security

- Require every Lease, Workspace, and idempotency lookup to include Tenant Scope.
- Return identical `404 LEASE_NOT_FOUND` responses for unknown and cross-scope Lease IDs.
