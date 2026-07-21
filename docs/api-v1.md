# HTTP contract v1

All protected endpoints accept `Authorization: Bearer <token>` and JSON bodies up to 1 MiB.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Public liveness check |
| `POST` | `/v1/sandboxes/acquire` | Create or reuse a ready sandbox by `(pool, key)` |
| `GET` | `/v1/sandboxes/:id` | Inspect lifecycle state |
| `POST` | `/v1/sandboxes/:id/exec` | Execute a command |
| `POST` | `/v1/sandboxes/:id/files/read` | Read UTF-8 or base64 content |
| `POST` | `/v1/sandboxes/:id/files/write` | Write UTF-8 or base64 content |
| `POST` | `/v1/sandboxes/:id/release` | End active use without immediate deletion |
| `DELETE` | `/v1/sandboxes/:id` | Permanently delete the sandbox |

## Identity

`key` is supplied by a consumer and should be stable for the desired reuse scope, such as a mikan conversation plus actor. `pool` selects platform capacity policy, not a Kubernetes object directly.

## Paths

Consumers see `/workspace`. Backends translate that path into their own storage implementation.

## Errors

Errors use:

```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "Sandbox 'sbx_123' does not exist"
  }
}
```

The code is stable for programmatic handling. The message is diagnostic text.
