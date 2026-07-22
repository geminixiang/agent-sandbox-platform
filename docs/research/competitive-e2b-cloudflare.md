# Competitive report: E2B open source and Cloudflare Sandbox SDK

- Date: 2026-07-22
- Core competitive target: the **verifiably open-source E2B offering**
- Secondary design reference: Cloudflare Sandbox SDK
- Product principles used for evaluation:
  1. Kubernetes deployment must be simple across GKE, EKS, AKS, macOS Colima/k3s, and Linux k3s.
  2. The Python SDK must become the reference interaction model before TypeScript and Go SDK parity work.

## Revisions reviewed

| Project | Commit |
| --- | --- |
| Agent Sandbox Platform | `5bbcf5c390db60a43c72750d87a1572efd0dc431` |
| E2B client/protocol monorepo | `be1ffa19f6ad6d7b1003d714ce45faf3cf2c3e21` |
| E2B infrastructure | `8b029108271d21273fab388146b9fe6b9fc547e8` |
| Cloudflare Sandbox SDK | `9ea53cfb7af0a7c0e586ba18cd4ce56e0295f594` |

This report considers only capabilities verifiable in those repositories. It does not infer internals of E2B Cloud or Cloudflare's hosted platform.

## Executive conclusion

E2B must be treated as a serious open-source infrastructure competitor. Its main `e2b` repository contains polished Python and JavaScript SDKs, CLI, contracts, template tooling, integration tests, and release automation. Its separate `infra` repository contains a substantial backend: API server, scheduler/orchestrator, per-host Firecracker lifecycle, guest agent, proxy, template builder, pause/resume and snapshots, persistence, auth, quotas, telemetry, Terraform, Nomad jobs, and tests.

E2B's weakness is not technical depth; it is operational complexity and portability. The most mature self-hosted path is a purpose-built GCP Nomad/Consul deployment. AWS is explicitly beta, local Linux is development-oriented, Azure is absent, and Kubernetes is not the production orchestration model. A production install spans Terraform, custom images, KVM hosts, Nomad/Consul, PostgreSQL, Redis, ClickHouse, object storage, registry, TLS, and Cloudflare-related networking inputs.

Our current platform has a credible but much narrower core: a Go Kubernetes control plane, Agent Sandbox/gVisor integration, strong Tenant Scope semantics, restart recovery, expiry cleanup, bounded exec, workspace file I/O, and an async-first Python SDK. The real competitive wedge is:

> Comparable isolation and agent workflows, delivered as a transparent Kubernetes-native platform that is dramatically easier to install and operate across clouds and locally.

We are not yet at parity. The Helm distribution is a placeholder, Python lacks essential process/filesystem/network workflows, quota is not present in the Go production path, and only Colima/k3s has a real end-to-end validation.

## What is actually open source

### E2B

Both E2B repositories are Apache-2.0 licensed (`e2b/LICENSE:1-202`; `e2b-infra/LICENSE:1-202`).

The `e2b` repository provides:

- Python and JavaScript SDKs (`e2b/packages/python-sdk`; `e2b/packages/js-sdk`).
- CLI and template build experience (`e2b/packages/cli`; `e2b/templates`).
- OpenAPI and protobuf contracts (`e2b/spec`).
- Integration and release automation (`e2b/packages/*/tests`; `e2b/.github/workflows`).
- A simple Python quick start: `with Sandbox.create()` then `sandbox.commands.run(...)` (`e2b/README.md:53-59`; `e2b/packages/python-sdk/README.md:35-41`).

The `e2b-infra` repository is not a thin launcher. It implements the public backend and self-hosting system, including:

- API/control-plane services and database migrations (`e2b-infra/packages/api`; `e2b-infra/packages/db/migrations`).
- Orchestrator/node placement and host daemon paths (`e2b-infra/packages/orchestrator`; `e2b-infra/packages/envd`).
- Firecracker-based sandbox lifecycle (`e2b-infra/packages/orchestrator/internal/sandbox`; `e2b-infra/packages/shared/pkg/grpc/orchestrator`).
- Edge proxy and sandbox traffic routing (`e2b-infra/packages/proxy`).
- Template building and artifact storage (`e2b-infra/packages/template-manager`; `e2b-infra/packages/api/internal/template`).
- Pause/resume, snapshots, persistence, quotas, telemetry, Terraform, and Nomad deployment definitions (`e2b-infra/terraform`; `e2b-infra/nomad`; `e2b-infra/self-host.md`).

E2B's own README directs self-hosters to Terraform and lists AWS/GCP while Azure and general Linux are unchecked (`e2b/README.md:87-95`). The infra implementation shows GCP as the clearest mature production path, AWS as beta, and a single-host Linux route as work in progress (`e2b-infra/self-host.md`; `e2b-infra/terraform/aws`; `e2b-infra/terraform/gcp`; `e2b-infra/local`).

### Cloudflare Sandbox SDK

Cloudflare's repository is Apache-2.0 (`cloudflare-sandbox-sdk/LICENSE:1-4`) and contains a TypeScript SDK, Durable Object integration, container-side runtime, images, protocol, tests, and examples. It does **not** provide an independently deployable substitute for Cloudflare Containers, Durable Objects, Workers, routing, or placement.

Its own architecture states the dependency on Cloudflare Containers, Durable Objects, VM isolation, and edge distribution (`cloudflare-sandbox-sdk/docs/ARCHITECTURE.md:145-158`). It is therefore a useful SDK/runtime design reference, but not the core open-infrastructure competitor.

Our repository is MIT licensed (`LICENSE:1-21`).

## Capability matrix

Legend: **Yes** = implemented and verifiable; **Partial** = narrow, immature, or depends on significant manual setup; **No** = absent.

| Capability | This platform | E2B open source | Cloudflare repo |
| --- | --- | --- | --- |
| Inspectable production control plane | Yes, early | Yes, substantial | No independent platform |
| Secure runtime | Agent Sandbox + gVisor; Kata target | Firecracker microVM | Hosted Cloudflare VM dependency |
| Production Kubernetes model | Yes, core model | No | No |
| GCP deployment | Target only | Mature Terraform/Nomad path | Cloudflare-managed |
| AWS deployment | Target only | Beta | Cloudflare-managed |
| Azure deployment | Target only | No | Cloudflare-managed |
| Local secure E2E | Colima/k3s + gVisor | Single-host Firecracker dev path | Docker is not equivalent to hosted isolation |
| One-command installation | No | No; multi-stage infrastructure | Hosted workflow is simple |
| Python SDK | Async lifecycle/files/run | Mature sync + async | No first-class Python SDK here |
| Streaming exec | No | Yes | Yes, SSE |
| Background processes/wait/kill | No | Yes | Yes |
| stdin/PTY/session semantics | No | Yes | Yes |
| Rich filesystem API | Basic read/write | Yes | Yes |
| Large-file streaming | No; JSON body capped | Yes | Richer runtime path |
| Authenticated port URLs | No | Yes | Yes |
| Templates/images | Operator Pool mapping only | Mature template build/version UX | Container image/Worker model |
| Pause/resume/snapshots | No | Yes | Sleep/restart semantics, not equivalent |
| Persistent volumes | PVC beneath Pool, no user contract | Implemented with provider caveats | R2/bucket-related features |
| Tenant isolation model | Explicit `(Consumer, Subject)` | Team/API-key model | Durable Object/application-defined naming |
| Quota in current production core | No | Yes | Platform limits plus SDK/runtime controls |
| Observability | Logs/tests only | Metrics/logging/telemetry systems | SDK/runtime tracing and tests |
| Helm/operator distribution | No | No | Not applicable |

## Where E2B is stronger

### 1. Python product ergonomics

E2B's reference workflow is shorter and synchronous by default:

```python
from e2b import Sandbox

with Sandbox.create() as sandbox:
    result = sandbox.commands.run('echo "Hello from E2B!"')
```

Source: `e2b/README.md:53-59` and `e2b/packages/python-sdk/README.md:35-41`.

Our first SDK requires an explicit client, base URL, credential provider, async context, and Pool (`packages/sdk-python/README.md:5-16`). That explicitness is appropriate for self-hosting but not yet ideal ergonomics. More importantly, E2B supports sync and async clients, reconnecting to running sandboxes, command streaming, background processes, timeouts, kill/wait, richer files, listing, metadata, and host/port URLs across its Python modules (`e2b/packages/python-sdk/e2b/sandbox_sync`; `e2b/packages/python-sdk/e2b/sandbox_async`).

### 2. Runtime lifecycle depth

E2B infra manages Firecracker VMs, host placement, node health, pause/resume, snapshots, filesystem artifacts, network proxying, and recovery. This is a much deeper runtime implementation than our current use of Agent Sandbox claims.

This does not make our adapter approach wrong. It means Agent Sandbox and the runtime backend must be treated as a critical dependency with explicit versioning, threat model, conformance suite, upgrade testing, and operational ownership.

### 3. Template workflow

E2B has a mature developer flow for defining, building, caching, tagging, uploading, and selecting sandbox templates. Our Pool mapping is useful for operator-controlled policy, but users cannot yet bring reproducible dependencies without an out-of-band image pipeline.

### 4. Networking

E2B offers per-sandbox hostnames/ports and proxy routing. Cloudflare similarly treats exposed ports and capability tokens as first-class (`cloudflare-sandbox-sdk/docs/ARCHITECTURE.md:123-143`). Our public contract has no networking surface.

### 5. Operational completeness

E2B infra includes auth/team models, quotas, migrations, telemetry, stateful dependencies, Terraform, Nomad jobs, and extensive runtime tests. Our production composition exists, but no image/chart/RBAC/NetworkPolicy/upgrade workflow exists (`deploy/helm/README.md:1-3`).

## Where we can win

### 1. Kubernetes-native multi-cloud operability

E2B self-hosting is powerful but operationally heavy. A standard Kubernetes distribution can reuse familiar operator infrastructure:

- Helm/OCI artifacts;
- Gateway API or Ingress;
- cert-manager and ExternalDNS;
- CSI storage;
- Secrets and workload identity;
- NetworkPolicy;
- Prometheus/OpenTelemetry;
- HPA and node autoscalers;
- GKE/EKS/AKS/k3s operational skills.

The promise is credible only after install and upgrade tests pass on each target. Architecture intent alone is not support (`docs/architecture.md:30-32`).

### 2. Security and tenancy by construction

Our Tenant Scope model is already unusually explicit:

- every operation receives `(Consumer, Subject)`;
- possession of Lease ID is not authorization;
- cross-scope and unknown IDs return identical 404 responses;
- idempotency is scope-bound;
- Kubernetes metadata uses HMAC-derived hashes rather than raw identities.

Sources: `docs/architecture.md:9-13,36-45`; `docs/api-v1.md:3-13`; `apps/control-plane-go/internal/backend/kubernetes/identity.go`; `apps/control-plane-go/internal/backend/kubernetes/backend.go`.

This is a differentiator for shared enterprise platforms, but it still needs OIDC/JWT verification, key rotation, audit events, real admission quota, and policy decisions.

### 3. Operator-managed Pools

E2B emphasizes developer-owned templates. We can offer a complementary enterprise model: the operator owns runtime images, isolation class, CPU/memory/storage, network policy, warm capacity, maximum TTL, and allowed capabilities; agents select only `pool="coding"`.

This reduces supply-chain and runtime-policy risk. It should not preclude an eventual OCI/template workflow with operator admission.

### 4. Transparent conformance

A public conformance suite that installs the platform and runs SDK workflows without vendor credentials would be a strong open-source differentiator. The existing Colima test already proves TypeScript SDK → Go control plane → k3s → Agent Sandbox → gVisor (`tests/e2e/kubernetes-agent-sandbox.mjs`; `deploy/colima/README.md:12-22`). It must be extended to built Python wheels and cloud install matrices.

## Product-principle assessment

### Principle 1: Kubernetes deployment must be simple

Current grade: **D**.

What exists:

- Kubernetes-only Go executable and backend;
- real Colima/k3s/gVisor E2E;
- documented environment variables and Pool mapping.

What is missing:

- control-plane container image;
- Helm chart;
- ServiceAccount/RBAC;
- Service, probes, Secret/ConfigMap;
- SandboxTemplate/WarmPool chart resources;
- NetworkPolicy;
- image signing/SBOM;
- preflight checks;
- install/status/smoke/uninstall command;
- upgrade/rollback tests;
- GKE/EKS/AKS/k3s matrix.

The local guide assumes the cluster already has Agent Sandbox CRDs/controller and gVisor (`deploy/colima/README.md:3-12`). This is not yet simple deployment.

Target UX:

```bash
helm install sandbox oci://ghcr.io/<org>/agent-sandbox-platform \
  --namespace sandbox-system --create-namespace \
  -f values.yaml

sandboxctl doctor
sandboxctl smoke-test
```

For local use, one wrapper should bootstrap or validate Colima/k3s, install dependencies, install the same chart, and print endpoint/credentials. The wrapper must not create a second local execution backend.

### Principle 2: Python SDK must define the interaction model

Current grade: **C-**.

Strengths:

- async context management;
- credential provider seam;
- typed records/errors;
- text/binary files;
- `run()`;
- strict pyright, wheel build, and clean install/import gate.

Sources: `packages/sdk-python/src/agent_sandbox/_client.py:17-195`; `packages/sdk-python/src/agent_sandbox/_errors.py`; `.github/workflows/ci.yml`.

Missing for a credible agent SDK:

- sync facade;
- environment-based configuration;
- Python wheel → real Go/k3s E2E;
- streaming output;
- stdin;
- background process handles and reconnect/wait/kill;
- PTY/terminal resize;
- list/stat/exists/mkdir/remove/rename;
- streaming upload/download beyond the 1 MiB JSON contract;
- list/connect sandboxes;
- metadata/tags;
- port exposure;
- retry guidance and richer timeout/cancellation semantics.

Target quick start should approach:

```python
from agent_sandbox import Sandbox

with Sandbox.create() as sandbox:
    result = sandbox.commands.run("python main.py")
```

An explicit `SandboxClient` must remain available for custom endpoint, credential provider, and multi-tenant server integrations.

## Prioritized roadmap

### P0 — establish a credible product

1. **Correct claims before adding features.** README currently claims a process backend and atomic quotas even though the Go production path has neither (`README.md:15-30`). Architecture also describes quota checks not implemented in Go (`docs/architecture.md:34-38`).
2. **Ship a production image and Helm chart.** Include ServiceAccount/RBAC, Deployment fixed to one replica, Service, probes, Secret references, Pool resources, Pod security settings, and NetworkPolicy.
3. **Make readiness real.** `/ready` must check startup recovery, Kubernetes access, required CRDs, configured WarmPools, and runtime prerequisites; it cannot be an unconditional 200.
4. **Add installation conformance.** Automated install, smoke, restart, upgrade, rollback, and uninstall tests on k3s/Colima; then GKE, EKS, AKS.
5. **Run the built Python wheel against real Kubernetes.** The release path must be wheel → clean install → Go control plane → Agent Sandbox → gVisor.
6. **Finish the command model.** Add streaming stdout/stderr, stdin, background process handles, wait, kill, reconnect, and cancellation.
7. **Finish the filesystem model.** Add list/stat/exists/mkdir/remove/rename and streaming large-file transfer.
8. **Implement real single-replica quota.** Tenant Scope, Consumer, Pool, concurrent-create, start-rate, and resource ceilings; hard-enforce one replica until distributed admission exists.
9. **Harden authentication.** OIDC/JWT issuer configuration, short-lived token broker guidance, secret rotation, audit events, and no giant JSON secret map as the long-term production model.

### P1 — match E2B's developer-critical surface

1. Python sync facade and `from_env()`/default Pool ergonomics.
2. List/connect/reuse Sandbox plus metadata/tags.
3. Authenticated per-Sandbox port URLs and revocation.
4. Egress policy: deny-all, domain/CIDR allowlists, DNS semantics, runtime updates.
5. OCI/template workflow with build logs, immutable versions, cache, registry integration, signing/SBOM/provenance.
6. Persistent workspace and volume lifecycle contract.
7. Metrics for CPU, memory, disk, start latency, active leases, failures, and tenant usage.
8. OpenAPI as canonical contract; generated or conformance-tested SDK models.
9. TypeScript and Go SDK parity only after Python semantics stabilize.

### P2 — advanced parity or differentiation

1. Pause/resume and auto-pause.
2. Filesystem snapshots and fork/checkpoint after runtime semantics are proven.
3. Workload identity brokerage for AWS/GCP/Azure.
4. Dedicated node pools, topology, isolation compatibility routing, and capacity-aware autoscaling.
5. Artifact retention, garbage collection, soft deletion, and audit controls.
6. Multi-replica coordination and multi-cluster placement.
7. Dashboard, team/member administration, SSO/SCIM, billing, marketplace.

## Deliberate non-goals for the current phase

- E2B hosted cloud scale or global scheduler;
- billing, marketplace, and polished dashboard;
- host-process execution backend;
- provider-specific concepts in SDKs;
- built-in notebook/code-interpreter rich outputs;
- MCP gateway;
- memory-preserving pause/resume;
- snapshot/fork before core command/files/network/deployment behavior is reliable;
- Cloudflare Durable Object or Worker concepts in the public contract.

## Ideas to borrow from Cloudflare

Cloudflare is the stronger secondary reference for hosted TypeScript ergonomics, not open deployment. Useful ideas include:

- domain-oriented SDK modules such as files, processes, and sessions;
- stable Sandbox identity separated from runtime generation;
- fencing stale runtime operations;
- idempotent destroy with explicit cleanup ordering;
- session-scoped cwd/env and serialized commands;
- streaming/non-streaming variants;
- typed error mapping;
- capability-token port exposure and revocation;
- credential brokerage instead of long-lived secret injection;
- E2E, browser, package, and performance release gates.

Sources: `cloudflare-sandbox-sdk/docs/ARCHITECTURE.md:69-87,101-143`; `cloudflare-sandbox-sdk/docs/SESSION_EXECUTION.md`; `cloudflare-sandbox-sdk/docs/ERROR_HANDLING.md`; `cloudflare-sandbox-sdk/docs/E2E_TESTING.md`.

Avoid copying:

- Durable Object/Worker terms into the public contract;
- sessions as a tenant security boundary;
- vendor tunnel/DNS as portable core;
- persistence semantics coupled to one object store;
- Docker-local execution as evidence of production isolation.

## Competitive scorecard and success criteria

The goal should not be “feature count equals E2B.” The near-term win condition is:

### Deployment

- A new operator installs on supported Kubernetes with one documented command and one values file.
- Preflight failures identify missing runtime, CRDs, storage, permissions, and capacity.
- The same image/chart passes install and lifecycle conformance on Colima/k3s, Linux k3s, GKE, EKS, and AKS.
- Upgrades and uninstall behavior are tested and documented.

### Python

- A new developer can install a wheel, configure endpoint/token from environment, create a Sandbox, stream a command, transfer files, expose a port, and clean up without learning Lease or Kubernetes concepts.
- Sync and async paths have equivalent semantics.
- Typed failures cover auth, quota, expiry, command exit, timeout, cancellation, files, and network exposure.
- Every release installs into a clean environment and runs against a real self-hosted cluster.

### Open operability

- Control plane, deployment, runtime adapter, policy, and conformance tests remain inspectable.
- Threat model and backend isolation guarantees are explicit.
- No vendor account is required for the local conformance path.
- Prometheus/OpenTelemetry and audit logs make lifecycle decisions observable.

## Final recommendation

Treat E2B as the benchmark for runtime depth, SDK breadth, templates, and lifecycle—not as a deployment model to copy. Treat Cloudflare as a reference for polished domain APIs, streaming, sessions, port capabilities, and lifecycle fencing.

Focus the next two milestones exclusively on the two product principles:

1. **Installable Kubernetes product:** image + Helm + RBAC + real readiness + k3s conformance.
2. **Reference Python workflow:** real-cluster wheel E2E + sync/async commands + processes + files + streaming.

If those milestones are executed well, the platform will have a defensible open-source position before matching E2B's snapshots, volumes, or global scheduling.
