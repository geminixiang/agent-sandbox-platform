# ADR 0001: Separate the sandbox platform from mikan

- Status: Accepted
- Date: 2026-07-21

## Decision

Maintain sandbox infrastructure, control-plane lifecycle, deployment, and SDK publishing in this standalone monorepo. Mikan consumes only the published HTTP SDK through a thin sandbox adapter.

## Consequences

- Mikan installation and startup do not depend on Kubernetes packages.
- Platform infrastructure can release independently.
- The HTTP `/v1` contract is the compatibility seam.
- SDK package verification is mandatory before publishing.
- Kubernetes implementation details remain private to the control plane.
