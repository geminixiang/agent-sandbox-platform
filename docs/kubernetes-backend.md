# Kubernetes Agent Sandbox backend

HTTP liveness checks only the process. Readiness traverses the Kubernetes backend, lists Claims, and verifies every configured WarmPool has a ready replica.

The Kubernetes backend maps each Platform Lease to one `SandboxClaim`. Kubernetes resource names, WarmPool names, RuntimeClass values, Pods, and PVCs remain private implementation details.

## Requirements

- Kubernetes with Agent Sandbox `v1beta1` CRDs and controller
- One or more `SandboxWarmPool` resources
- A configured RuntimeClass such as `gvisor` or `kata-qemu`
- Control-plane credentials with SandboxClaim, Sandbox, Pod read, Pod exec, and Claim delete permissions
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
go run ./apps/control-plane-go/cmd/control-plane
```

`SANDBOX_KUBECONFIG` is optional; standard in-cluster configuration and kubeconfig loading are supported. There is no process backend or host-execution fallback.

The Consumer sends only `pool: "coding"`; the WarmPool and RuntimeClass mapping is server-side.

## Metadata and recovery

Claims contain only HMAC-derived hashes for Consumer, Tenant Scope, idempotency, and logical Pool. Raw Consumer IDs, Subject IDs, and idempotency keys are never stored in Kubernetes metadata.

On startup the control plane:

1. lists managed Claims,
2. deletes expired Claims,
3. reconstructs active Lease records for quota accounting and lookup.

Changing `SANDBOX_METADATA_SECRET` makes existing Claims inaccessible to their original Tenant Scope. Treat it as persistent state.

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

The test starts the production Go control plane and drives it through the published TypeScript SDK. It verifies acquire, gVisor runtime, exec, files, control-plane restart recovery, release, and cleanup.
