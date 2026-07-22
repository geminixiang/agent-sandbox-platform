# Kubernetes Agent Sandbox backend

HTTP liveness checks only the process. Readiness traverses the Kubernetes backend, lists Claims and Sandboxes, and verifies every configured WarmPool has all desired replicas ready.

The Kubernetes backend maps each Platform Lease to one `SandboxClaim`. Kubernetes resource names, WarmPool names, RuntimeClass values, Pods, and PVCs remain private implementation details.

## Requirements

- Kubernetes with Agent Sandbox `v1beta1` CRDs and controller
- One or more `SandboxWarmPool` resources
- A configured RuntimeClass such as `gvisor` or `kata-qemu`
- Control-plane credentials with SandboxClaim, Sandbox, Pod read, Pod exec, and Claim delete permissions
- Sandbox workload images that provide `/usr/local/bin/agent-sandbox-transfer` with the ASP1 protocol described below
- A stable `SANDBOX_METADATA_SECRET`

The current quota lock is process-local, so run exactly **one control-plane replica**. Multi-replica deployment requires a distributed acquisition lock and is intentionally not implemented yet.

## Configuration

The Go control plane is Kubernetes-only and reads the following environment variables:

```bash
export SANDBOX_K8S_CONTEXT=colima-agent-sandbox-gvisor
export SANDBOX_K8S_NAMESPACE=agent-sandbox-platform-e2e
export SANDBOX_METADATA_SECRET='replace-with-a-stable-random-secret'
export SANDBOX_CONSUMER_SECRETS='{"mikan":"replace-with-consumer-secret"}'
export SANDBOX_K8S_POOLS='{
  "coding": {
    "warmPoolName": "platform-gvisor",
    "runtimeClassName": "gvisor",
    "containerName": "shell"
  }
}'
export SANDBOX_SWEEP_INTERVAL=30s
export SANDBOX_FILE_TRANSFER_MAX_CONCURRENT=8
export SANDBOX_FILE_TRANSFER_MAX_PER_LEASE=2
export SANDBOX_FILE_TRANSFER_TIMEOUT=2m
go run ./apps/control-plane-go/cmd/control-plane
```

`SANDBOX_KUBECONFIG` is optional; standard in-cluster configuration and kubeconfig loading are supported. There is no process backend or host-execution fallback.

The Consumer sends only `pool: "coding"`; the WarmPool and RuntimeClass mapping is server-side. Transfer settings must be positive, and the per-Lease limit cannot exceed the global limit. The timeout is a total transfer deadline capped by Lease expiry. A separate idle timeout is not implemented or claimed yet.

## Sandbox runtime transfer contract

Production Pool images must install the repository's static `agent-sandbox-transfer` helper at `/usr/local/bin/agent-sandbox-transfer`. The control plane invokes it directly over Pod exec; SDKs never see Kubernetes or helper details.

- `download <absolute-workspace-path>` securely opens a regular file without following symlinks, makes a bounded snapshot, computes its exact length and SHA-256, emits one bounded `ASP1 OK <size> <hex-digest>` marker, then streams exactly that snapshot.
- `upload <absolute-workspace-path> <size> <hex-digest>` reads the exact body, rejects short/long or digest-mismatched payloads, and atomically renames a validated sibling temporary file over the destination before emitting `ASP1 OK`.
- Failures emit only `ASP1 ERR <stable-code>`. Marker and diagnostic buffers are bounded. Temporary names, raw utility output, Pod names, and Kubernetes errors are not exposed through HTTP.
- Every path component is opened relative to a pinned `/workspace` directory descriptor with no-follow semantics. Missing download files map to `FILE_NOT_FOUND`; symlinks, directories, and escapes map to `INVALID_PATH`; files above 64 MiB fail before HTTP `200` with `TRANSFER_TOO_LARGE`.

The published coding and browser images include this helper. Operator-supplied Pool images must satisfy the same runtime contract.

## Metadata and recovery

Claims contain only HMAC-derived hashes for Consumer, Tenant Scope, idempotency, and logical Pool. Raw Consumer IDs, Subject IDs, and idempotency keys are never stored in Kubernetes metadata.

On startup the control plane:

1. lists managed Claims,
2. deletes expired Claims,
3. reconstructs active Lease records for quota accounting and lookup.

Changing `SANDBOX_METADATA_SECRET` makes existing Claims inaccessible to their original Tenant Scope and invalidates outstanding discovery cursors. Treat it as persistent state.

## Active discovery and cursors

Discovery lists SandboxClaims with exact server-side labels for `managed=true`, the authenticated Tenant Scope hash, and, when requested, the logical Pool hash. It passes the requested limit and opaque Kubernetes continuation directly to the API server. The backend does not re-sort Claims or fetch extra raw pages to fill pages after active filtering; an empty page with a continuation is therefore expected and safe.

The first page fixes an `asOf` time. Every page excludes Claims with a deletion timestamp or `expiresAt <= asOf`. This stabilizes active evaluation across a traversal without pretending that discovery is a transaction: release can still win a race after a page is returned.

Public cursors are AES-256-GCM envelopes. Their key is deterministically derived from `SANDBOX_METADATA_SECRET` with the `lease-list-cursor/v1` HMAC-SHA256 domain; each cursor uses a random nonce and fixed authenticated data. The encrypted payload binds cursor version, Tenant Scope digest, Pool digest or no-Pool marker, limit, fixed `asOf`, and the raw Kubernetes continuation. No second cursor secret is configured. Cursors survive a control-plane restart with the same metadata secret. Expired Kubernetes continuations map to `CURSOR_EXPIRED`; malformed, tampered, or context-mismatched envelopes map uniformly to `INVALID_CURSOR`.

## Cleanup

- `release` deletes the Claim with foreground propagation.
- Lease expiry is written to Claim `shutdownTime` with `DeleteForeground` and is also enforced by a periodic control-plane sweep.
- Claims with `deletionTimestamp` are immediately excluded from lookup, idempotency replay, recovery, and quota counts.

## Colima E2E

```bash
kubectl --context colima-agent-sandbox-gvisor apply -f deploy/colima/e2e.yaml
SANDBOX_E2E_KUBECONTEXT=colima-agent-sandbox-gvisor npm run test:e2e:kubernetes
kubectl --context colima-agent-sandbox-gvisor delete namespace agent-sandbox-platform-e2e
```

The reusable acceptance harness starts the production Go control plane and drives it through the published TypeScript SDK. It verifies two-Subject isolation, Pool-filtered limit-1 discovery, cross-scope cursor rejection, cursor continuation after a same-secret control-plane restart, connect/exec, files, release, and Claim cleanup.
