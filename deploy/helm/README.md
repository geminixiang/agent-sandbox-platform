# Helm deployment

This chart installs the production Go control plane and operator-defined Sandbox Pools. It deliberately does **not** vendor the Agent Sandbox controller or runtime installation.

## Prerequisites

The target cluster must already have:

- Agent Sandbox `v0.5.2` CRDs/controller with extensions;
- every `RuntimeClass` referenced by an enabled Pool;
- nodes capable of running those RuntimeClasses;
- a default StorageClass;
- the control-plane image available to the cluster.

A Helm `lookup` preflight verifies the CRDs and RuntimeClasses against the target cluster before rendering install resources. Installation fails before creating Platform workloads when prerequisites are absent. Set `preflight.enabled=false` only for offline `helm template` rendering.

## Secrets

Create a Secret in the release namespace. Do not put secret values in values files:

```bash
kubectl -n agent-sandbox-platform create secret generic agent-sandbox-platform-secrets \
  --from-literal=SANDBOX_METADATA_SECRET="$(openssl rand -hex 32)" \
  --from-literal=SANDBOX_CONSUMER_SECRETS='{"mikan":"replace-me"}'
```

`SANDBOX_METADATA_SECRET` is persistent identity state: rotating it makes existing Claims inaccessible to their original Tenant Scope.

## Install

```bash
helm upgrade --install agent-sandbox-platform ./deploy/helm \
  --namespace agent-sandbox-platform \
  --create-namespace \
  --values values.production.yaml \
  --wait
```

Minimal override:

```yaml
image:
  repository: ghcr.io/example/agent-sandbox-control-plane
  tag: 0.1.0

existingSecret: agent-sandbox-platform-secrets

pools:
  coding:
    warmPoolName: platform-coding
    runtimeClassName: gvisor
    containerName: shell
    enabled: true
    replicas: 1
    image: alpine:3.22
    command: ["sh", "-c", "trap 'exit 0' TERM; while true; do sleep 3600; done"]
    workspaceSize: 1Gi
    podAnnotations: {}
    nodeSelector: {}
    tolerations: []
    resources:
      requests: { cpu: 10m, memory: 16Mi }
      limits: { cpu: 500m, memory: 256Mi }
```

The chart enforces exactly one control-plane replica until distributed acquisition coordination is implemented. The Deployment uses `Recreate` to prevent rollout overlap.

## Security defaults

- non-root distroless control plane;
- read-only control-plane root filesystem;
- no privilege escalation and all capabilities dropped;
- namespace-scoped RBAC for Claims, Sandboxes, Pods, and Pod exec;
- existing Secret reference rather than generated secret data;
- ingress NetworkPolicy enabled by default;
- non-root Pool workloads with default seccomp.

Pool RuntimeClass and images remain operator policy; SDK consumers only select logical Pool names.

## Verify

```bash
helm lint deploy/helm
helm template platform deploy/helm --namespace agent-sandbox-platform >/tmp/platform.yaml
kubectl apply --dry-run=client -f /tmp/platform.yaml
```
