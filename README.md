# Agent Sandbox Platform

Standalone sandbox control plane and client SDK for AI agents.

## Responsibility

This repository owns sandbox infrastructure and lifecycle:

- acquisition, leases, release, and deletion
- command and file transports
- authentication and quotas
- Kubernetes Agent Sandbox integration
- runtime isolation, images, deployment, and observability

Consumers such as mikan only use the HTTP SDK. They must not depend on Kubernetes clients, CRDs, Helm, Kata, gVisor, or machine provisioning.

```text
mikan → @geminixiang/sandbox-sdk → HTTP control plane → sandbox backend
```

## Current phase

Phase 0 provides a stable HTTP seam and a local process backend. The local backend is for development and contract tests only. **It does not isolate untrusted code.** Kubernetes Agent Sandbox support will be added behind the same backend interface later.

## Packages

- `@geminixiang/sandbox-contracts`: protocol constants and documentation
- `@geminixiang/sandbox-sdk`: zero-runtime-dependency HTTP SDK
- `@geminixiang/sandbox-control-plane`: private HTTP server

## Quick start

```bash
npm install
SANDBOX_API_TOKEN=dev npm start
```

In another terminal:

```bash
curl http://127.0.0.1:8787/health
curl -H 'authorization: Bearer dev' \
  -H 'content-type: application/json' \
  -d '{"key":"demo","pool":"local"}' \
  http://127.0.0.1:8787/v1/sandboxes/acquire
```

## Verify

```bash
npm test
npm run test:package
```

## Roadmap

1. Stabilize and version the HTTP contract.
2. Add a Kubernetes Agent Sandbox backend inside the control plane only.
3. Add Colima integration tests.
4. Add runtime/router images and Helm deployment.
5. Publish the SDK, then add a thin adapter to mikan.
