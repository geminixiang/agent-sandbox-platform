# GKE deployment profile

This directory contains operator overrides for installing the platform on an existing GKE cluster. It does not create or mutate a cluster.

## Supported baseline

- GKE Standard with a COS_CONTAINERD node pool created with `--sandbox type=gvisor`; or
- GKE Autopilot 1.27.4-gke.800 or later.

Google documents `runtimeClassName: gvisor` as the workload opt-in. Standard sandbox nodes carry `sandbox.gke.io/runtime=gvisor` and a matching `NoSchedule` taint; GKE also injects the needed affinity and toleration for gVisor Pods. The checked-in Standard profile states those constraints explicitly. Remove its Pool `nodeSelector` and `tolerations` for Autopilot.

Primary source: https://cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods

## Safe preflight

Choose the context explicitly. Never rely on the current context:

```bash
export GKE_CONTEXT='gke_PROJECT_LOCATION_CLUSTER'
kubectl --context "$GKE_CONTEXT" cluster-info
kubectl --context "$GKE_CONTEXT" get nodes -L sandbox.gke.io/runtime
kubectl --context "$GKE_CONTEXT" get runtimeclass gvisor
kubectl --context "$GKE_CONTEXT" get crd \
  sandboxes.agents.x-k8s.io \
  sandboxclaims.extensions.agents.x-k8s.io \
  sandboxtemplates.extensions.agents.x-k8s.io \
  sandboxwarmpools.extensions.agents.x-k8s.io
```

If Agent Sandbox is absent, install the pinned upstream core+extensions prerequisite separately:

```bash
kubectl --context "$GKE_CONTEXT" apply -f \
  https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v0.5.2/sandbox-with-extensions.yaml
```

The platform chart deliberately does not own those cluster-scoped CRDs/controllers.

## Install

Create credentials without writing their values to a values file:

```bash
namespace=agent-sandbox-platform
kubectl --context "$GKE_CONTEXT" create namespace "$namespace"
kubectl --context "$GKE_CONTEXT" -n "$namespace" create secret generic agent-sandbox-platform-secrets \
  --from-literal=SANDBOX_METADATA_SECRET="$(openssl rand -hex 32)" \
  --from-literal=SANDBOX_CONSUMER_SECRETS='{"replace-consumer":"replace-secret"}'

helm upgrade --install agent-sandbox-platform ./deploy/helm \
  --kube-context "$GKE_CONTEXT" \
  --namespace "$namespace" \
  --values deploy/gke/values-gvisor.yaml \
  --wait --timeout 10m
```

Do not run these commands against an existing cluster until its context, ownership, maintenance constraints, and acceptable changes have been confirmed.

## Daily-work acceptance test

The opt-in E2E profile temporarily gives the two Pools DNS plus access to a single labeled fixture Service while preserving public-internet egress and private-range exclusions. It tests crawler, Git/GitHub HTTPS, and Playwright form/popup/download workflows through the built Python wheel, then restores the secure-default GKE profile:

```bash
./scripts/gke/workload-smoke.sh
```

The script is context-pinned to `agent-sandbox-e2e` and reads local credentials from `.sandbox-platform/gke-e2e.json`. It must not be run against another cluster. The checked-in fixture sends no crawl load to third-party sites; external canaries are limited to one GitHub API request, one shallow clone of GitHub's `octocat/Hello-World`, and `example.com`.

Failure and recovery acceptance verifies typed command timeout, continued use of the same sandbox after cancellation, and the readiness transition when a Pool loses and regains all capacity:

```bash
./scripts/gke/failure-smoke.sh
```

Both scripts restore operator policy and Pool capacity from cleanup traps.

## Verify

```bash
kubectl --context "$GKE_CONTEXT" -n "$namespace" get deploy,pods,sandboxwarmpools
kubectl --context "$GKE_CONTEXT" -n "$namespace" port-forward service/agent-sandbox-platform 8787:8787
curl --fail http://127.0.0.1:8787/health
curl --fail http://127.0.0.1:8787/ready
```

A complete acceptance test must then acquire both `coding` and `browser` through the built Python wheel, confirm the sandbox runtime reports gVisor, run Playwright Chromium, release both leases, and verify Claim/Pod/PVC cleanup.
