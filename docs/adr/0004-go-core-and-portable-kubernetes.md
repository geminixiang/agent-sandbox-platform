# ADR 0004: Go core, language SDKs, and portable Kubernetes deployment

- Status: Accepted
- Date: 2026-07-22

## Context

The Platform must own multi-tenant Lease lifecycle and sandbox infrastructure while remaining easy to consume from agent applications written in different languages. It must run on managed Kubernetes in major clouds and on practical local Kubernetes environments without exposing provider-specific details through the Consumer interface.

The initial TypeScript control-plane prototype validated the `/v1/leases` contract, Tenant Scope enforcement, lifecycle rules, and Kubernetes feasibility. Continuing to grow that prototype into the production controller would trade short-term code reuse for weaker alignment with the Kubernetes controller ecosystem.

## Decision

1. Implement the production control plane and backend adapters in Go.
2. Publish TypeScript and Python SDKs as thin adapters over one language-neutral HTTP contract.
3. Keep Kubernetes, CRDs, Helm, routing, runtime selection, and cloud-provider integration behind the control-plane backend seam.
4. Support these deployment classes from one Kubernetes-oriented distribution:
   - GKE
   - Amazon EKS
   - Azure AKS
   - macOS using Colima and k3s
   - Linux Kubernetes, with k3s as the initial local reference
5. Do not ship a local process backend. Local deployment means running the Kubernetes backend on local Kubernetes (Colima with k3s on macOS, and k3s or an existing Kubernetes cluster on Linux).
6. Avoid cloud-provider SDKs in public SDK packages. Provider-specific capabilities belong in internal adapters and deployment configuration.

## Consequences

- The HTTP contract, not a language implementation, is the compatibility seam.
- Every protocol change requires contract, TypeScript SDK, Python SDK, and Go control-plane tests once both SDKs exist.
- The TypeScript SDK remains free of runtime dependencies; the Python SDK should likewise remain lightweight and transport-focused.
- Deployment portability is tested at the Kubernetes resource and behavior level. Provider-specific installation guidance may vary, but Lease semantics may not.
- The Go control plane must fail closed when Kubernetes is unavailable; there is no local-process fallback.
- Cloud identity, storage classes, ingress, load balancers, and runtime classes are deployment inputs rather than Consumer-visible protocol concepts.
