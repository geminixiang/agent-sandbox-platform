#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${ROOT}/deploy/helm"

helm lint "${chart}"
rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT
helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false >"${rendered}"
kubectl apply --dry-run=client -f "${rendered}" >/dev/null

if helm template platform "${chart}" --namespace agent-sandbox-platform --set preflight.enabled=false --set replicaCount=2 >/dev/null 2>&1; then
  echo "ERROR: chart accepted replicaCount=2" >&2
  exit 1
fi

grep -q 'kind: Deployment' "${rendered}"
grep -q 'runAsNonRoot: true' "${rendered}"
grep -q 'kind: NetworkPolicy' "${rendered}"
grep -q 'kind: SandboxWarmPool' "${rendered}"
echo "Helm chart verification passed"
