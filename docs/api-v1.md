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
| `GET` | `/v1/leases?pool=...&limit=...&cursor=...` | List active Leases in the caller's Tenant Scope |
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

`ttlSeconds` defaults to 900 and is currently capped at 3600 by the backend policy. `POST /v1/leases` accepts no query parameters.

## Active Lease discovery

`GET /v1/leases` lists only active Leases in the authenticated Tenant Scope. `pool` is an optional exact logical-Pool filter. `limit` defaults to 50 and must be from 1 through 100. `cursor` is an opaque continuation returned by the preceding page; query parameters cannot be blank, repeated, or unknown.

```json
{
  "leases": [],
  "nextCursor": "opaque-or-null"
}
```

`nextCursor` is always present and is `null` on the final page. An empty `leases` array with a non-null cursor is valid: filtering expired or deleting Claims can empty a raw Kubernetes page, so clients must continue until `nextCursor` is null. Results use Kubernetes' stable list order and are not recency ordered. In particular, `lastUsedAt` is not a recency-safe ordering key.

Active status is evaluated against an `asOf` time fixed by the first page and protected inside every subsequent cursor. A Lease is active when it has no deletion timestamp and `expiresAt > asOf`. Concurrent release can therefore remove a Lease after it was listed; connecting through `GET /v1/leases/:id` may legitimately return `LEASE_NOT_FOUND`.

Cursors are authenticated and encrypted, bind the Tenant Scope, Pool filter, limit, and fixed `asOf`, and are portable across control-plane restarts that retain the same metadata secret. Malformed, tampered, or cross-scope/filter/limit cursors return `400 INVALID_CURSOR`. An expired Kubernetes continuation returns `410 CURSOR_EXPIRED`. An unconfigured logical Pool returns `400 UNKNOWN_POOL` without disclosing operator mappings.

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

The production Kubernetes backend implements these streaming endpoints through the sandbox runtime transfer protocol. Legacy file methods remain available. Backends without the optional streaming interface may still return `501 STREAMING_NOT_SUPPORTED`; no local-process backend is described as secure isolation.

Transfers fail fast with `429 TRANSFER_LIMIT_REACHED` when either the global or per-Lease concurrency limit is full. The production defaults are 8 global and 2 per Lease. Each transfer has a total timeout (2 minutes by default), capped by Lease expiry. Cancellation, timeout, release, deletion, expiry, and backend shutdown abort active transfers with `408 ABORTED`. An idle timeout is intentionally not claimed in this release; the bounded total timeout is the enforced guarantee.

Upload validation codes are:

| Status | Code | Meaning |
| --- | --- | --- |
| `400` | `INVALID_REQUEST` | Missing, blank, or duplicated `path` query value |
| `400` | `INVALID_CONTENT_DIGEST` | Missing or non-canonical SHA-256 `Content-Digest` |
| `400` | `CONTENT_LENGTH_MISMATCH` | Body byte count differs from `Content-Length` |
| `411` | `LENGTH_REQUIRED` | No known `Content-Length` |
| `408` | `ABORTED` | Transfer was cancelled, timed out, or stopped by Lease/backend lifecycle |
| `404` | `FILE_NOT_FOUND` | Download source does not exist |
| `413` | `TRANSFER_TOO_LARGE` | Declared or preflighted transfer exceeds 64 MiB |
| `429` | `TRANSFER_LIMIT_REACHED` | Global or per-Lease transfer concurrency is full |
| `400` | `INVALID_PATH` | Path escapes the workspace, traverses a symlink, or is not a regular file |
| `502` | `FILE_TRANSFER_FAILED` | The sandbox runtime transfer protocol failed without exposing runtime details |
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
