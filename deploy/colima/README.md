# Colima development environment

The repository includes a disposable Agent Sandbox integration environment in `e2e.yaml`:

- namespace: `agent-sandbox-platform-e2e`
- RuntimeClass: `gvisor`
- SandboxTemplate and one-replica WarmPool
- Alpine runtime container
- 128 MiB workspace PVC

It assumes the `colima-agent-sandbox-gvisor` cluster already has Agent Sandbox CRDs/controller and the `gvisor` RuntimeClass.

```bash
kubectl --context colima-agent-sandbox-gvisor apply -f deploy/colima/e2e.yaml
SANDBOX_E2E_KUBECONTEXT=colima-agent-sandbox-gvisor npm run test:e2e:kubernetes
kubectl --context colima-agent-sandbox-gvisor delete namespace agent-sandbox-platform-e2e
```

The process backend is only for trusted contract development. Use the Kubernetes backend for isolation integration tests.
