# Agent Sandbox Platform

A shared sandbox control plane and lightweight SDK for AI agents.

## Responsibility

The Platform grants temporary Leases while hiding sandbox infrastructure and lifecycle details. Consumers such as mikan use only the HTTP SDK; they do not depend on Kubernetes clients, CRDs, Helm, Kata, gVisor, or machine provisioning.

```text
Consumer → @geminixiang/sandbox-sdk → HTTP control plane → sandbox backend
```

Mikan is the initial Primary Design Partner, not the only intended Consumer.

## Current phase

The platform provides:

1. temporary Lease rights rather than ownership of Pods, VMs, or Sandboxes,
2. `(Consumer, Subject)` isolation for every Lease and Workspace operation,
3. a production Go control plane backed only by Kubernetes Agent Sandbox,
4. server-side logical Pool mapping, runtime verification, restart recovery, and release/expiry cleanup,
5. async-first Python SDK, zero-runtime-dependency TypeScript SDK, and local pi tools,
6. reproducible Colima+k3s+gVisor deployment plus a production Helm chart.

Exactly one control-plane replica is supported. Admission quota enforcement and distributed acquisition coordination are not implemented yet.

## Packages

- `packages/sdk-python`: reference async-first Python SDK
- `packages/sdk-go`: standard-library-only Go SDK
- `@geminixiang/sandbox-sdk`: zero-runtime-dependency TypeScript HTTP SDK
- `@geminixiang/sandbox-contracts`: `/v1` protocol constants and types
- `apps/control-plane-go`: production Kubernetes-only Go control plane
- `.pi/extensions/agent-sandbox`: project-local pi tools

## Local quick start

The local Golden Path creates a Colima+k3s cluster, installs pinned Agent Sandbox prerequisites and gVisor, starts the Go control plane, and launches pi:

```bash
./scripts/local/pi-up.sh
```

See [`deploy/colima/README.md`](deploy/colima/README.md) for lifecycle and browser smoke tests. Mikan's trial starts with [`docs/trial/mikan-quickstart.md`](docs/trial/mikan-quickstart.md). For production, see [`deploy/helm/README.md`](deploy/helm/README.md).

## Kubernetes backend

See [`docs/kubernetes-backend.md`](docs/kubernetes-backend.md) for requirements, configuration, lifecycle, and the Colima integration test. Durable cloud and real-cluster evidence is indexed in [`docs/test-reports.md`](docs/test-reports.md).

The chart hard-enforces one replica until distributed acquisition and quota coordination are implemented.

## Verify

```bash
npm test
npm run test:package
go test ./...
(cd packages/sdk-go && go test -race ./...)
./scripts/check-go-sdk-module.sh
./scripts/check-helm.sh
(cd packages/sdk-python && uv run pytest && uv run pyright)
```

The test suite includes cross-Subject and cross-Consumer access attempts for inspect, exec, files, release, delete, and idempotency replay.

## Next milestone

Publish multi-architecture container images, validate the Helm chart on GKE with a gVisor Pool, then run real browser-heavy Python workloads. Billing, Channels, Restore, direct Firecracker management, and multi-cluster placement remain intentionally out of scope.
