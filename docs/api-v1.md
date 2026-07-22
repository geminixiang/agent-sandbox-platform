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
| `GET` | `/v1/leases/:id/files/content?path=...` | Stream bounded binary workspace content |
| `PUT` | `/v1/leases/:id/files/content?path=...` | Atomically stream bounded binary workspace content |
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

## Bounded binary transfers

The streaming endpoints have a fixed per-transfer limit of 64 MiB. They do not provide ranges, resume, compression, directory operations, or per-Lease storage quota. The legacy JSON/base64 endpoints remain available and unchanged.

A streaming upload requires all of the following headers:

```text
Content-Type: application/octet-stream
Content-Length: <exact byte count>
Content-Digest: sha-256=:<base64 of exactly 32 digest bytes>:
```

`Content-Digest` uses the RFC 9530 canonical representation. The backend must consume the body without whole-file buffering, verify the declared length and SHA-256 digest, and atomically replace the destination. Success is `204 No Content`.

A successful download is:

```text
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Length: <exact byte count>
Content-Digest: sha-256=:<base64 of exactly 32 digest bytes>:

<raw bytes>
```

Lease, scope, path, file-existence, size, and readability checks happen before the `200` response is committed, so those failures use the JSON error envelope. If the reader fails after the binary response starts, the server aborts the HTTP response and never appends JSON. Clients must validate length and digest after normal EOF; callers that intentionally close early are cancelling the transfer and do not perform integrity validation.

The current Kubernetes backend does **not** implement binary streaming yet. These endpoints return `501 STREAMING_NOT_SUPPORTED` for that backend. Legacy file methods continue to work. This contract does not claim Kubernetes streaming support or secure local-process isolation.

Upload validation codes are:

| Status | Code | Meaning |
| --- | --- | --- |
| `400` | `INVALID_REQUEST` | Missing, blank, or duplicated `path` query value |
| `400` | `INVALID_CONTENT_DIGEST` | Missing or non-canonical SHA-256 `Content-Digest` |
| `400` | `CONTENT_LENGTH_MISMATCH` | Body byte count differs from `Content-Length` |
| `411` | `LENGTH_REQUIRED` | No known `Content-Length` |
| `413` | `TRANSFER_TOO_LARGE` | Declared or preflighted transfer exceeds 64 MiB |
| `415` | `UNSUPPORTED_MEDIA_TYPE` | Upload is not `application/octet-stream` |
| `422` | `CONTENT_DIGEST_MISMATCH` | Body SHA-256 differs from `Content-Digest` |
| `501` | `STREAMING_NOT_SUPPORTED` | Backend does not implement the optional streaming interface |

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
