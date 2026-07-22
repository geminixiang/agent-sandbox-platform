# HTTP contract v1

## Authentication and Tenant Scope

All protected endpoints require a short-lived HMAC Subject token:

```text
Authorization: Bearer v1.<claims>.<signature>
```

The signed claims contain only opaque `consumerId`, `subjectId`, and `exp`. The server resolves the Consumer secret and derives the indivisible Tenant Scope `(Consumer, Subject)` from the verified token. Request bodies cannot declare or override that scope.

A resource outside the caller's Tenant Scope returns exactly the same `404 LEASE_NOT_FOUND` response as an unknown Lease ID.

## Endpoints

JSON request bodies are limited to 1 MiB.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Public liveness check |
| `POST` | `/v1/leases` | Create a temporary Lease |
| `GET` | `/v1/leases/:id` | Inspect Lease state |
| `POST` | `/v1/leases/:id/exec` | Execute through an active Lease |
| `POST` | `/v1/leases/:id/files/read` | Read UTF-8 or base64 workspace content |
| `POST` | `/v1/leases/:id/files/write` | Write UTF-8 or base64 workspace content |
| `POST` | `/v1/leases/:id/release` | Irreversibly relinquish the Lease |
| `DELETE` | `/v1/leases/:id` | Permanently delete retained resources |

Creating a Lease requires an `Idempotency-Key` header. Its mapping is scoped to the authenticated `(Consumer, Subject)`.

```json
{
  "pool": "coding",
  "ttlSeconds": 900
}
```

`ttlSeconds` defaults to 900 and is currently capped at 3600 by the backend policy.

## Paths

Consumers see `/workspace`. Backends translate that path into their own storage implementation.

## Errors

Errors use stable codes:

```json
{
  "error": {
    "code": "LEASE_NOT_FOUND",
    "message": "Lease not found"
  }
}
```
