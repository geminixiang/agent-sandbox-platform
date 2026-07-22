#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${ROOT}/deploy/helm"

helm lint "${chart}"
rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT
helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false >"${rendered}"

# helm lint and template validate chart/schema/rendering without requiring Kubernetes discovery.
# Real API validation is covered by the Colima Helm rollout smoke test.

if helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false --set replicaCount=2 >/dev/null 2>&1; then
  echo "ERROR: chart accepted replicaCount=2" >&2
  exit 1
fi

grep -q 'kind: Deployment' "${rendered}"
grep -q 'runAsNonRoot: true' "${rendered}"
grep -q 'kind: NetworkPolicy' "${rendered}"
grep -q 'kind: SandboxWarmPool' "${rendered}"
grep -q 'automountServiceAccountToken: false' "${rendered}"
grep -q 'name: SANDBOX_FILE_TRANSFER_MAX_CONCURRENT' "${rendered}"
grep -q 'name: SANDBOX_FILE_TRANSFER_MAX_PER_LEASE' "${rendered}"
grep -q 'name: SANDBOX_FILE_TRANSFER_TIMEOUT' "${rendered}"

if helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false --set controlPlane.fileTransfer.maxConcurrent=0 >/dev/null 2>&1; then
  echo "ERROR: chart accepted a zero file transfer limit" >&2
  exit 1
fi

gke_rendered="$(mktemp)"
trap 'rm -f "${rendered}" "${gke_rendered}"' EXIT
helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false --values "${ROOT}/deploy/gke/values-gvisor.yaml" >"${gke_rendered}"
grep -q 'sandbox.gke.io/runtime: gvisor' "${gke_rendered}"
grep -q 'kind: SandboxWarmPool' "${gke_rendered}"
grep -q 'name: platform-browser' "${gke_rendered}"

e2e_rendered="$(mktemp)"
trap 'rm -f "${rendered}" "${gke_rendered}" "${e2e_rendered}"' EXIT
helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false --values "${ROOT}/deploy/gke/values-gvisor.yaml" --values "${ROOT}/deploy/gke/values-workload-e2e.yaml" >"${e2e_rendered}"
grep -q 'dnsPolicy: ClusterFirst' "${e2e_rendered}"
grep -q 'app: workload-fixture' "${e2e_rendered}"
grep -q '169.254.0.0/16' "${e2e_rendered}"
echo "Helm chart verification passed"
