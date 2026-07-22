#!/usr/bin/env bash
set -euo pipefail

project="${SANDBOX_GKE_PROJECT:-glab-384109}"
zone="${SANDBOX_GKE_ZONE:-asia-east1-b}"
cluster="${SANDBOX_GKE_CLUSTER:-agent-sandbox-e2e}"
expected="delete ${project}/${zone}/${cluster}"
[[ "${project}/${zone}/${cluster}" == "glab-384109/asia-east1-b/agent-sandbox-e2e" ]] || {
  echo "ERROR: refusing non-E2E target ${project}/${zone}/${cluster}" >&2
  exit 1
}
[[ "${SANDBOX_GKE_DELETE_CONFIRM:-}" == "${expected}" ]] || {
  echo "ERROR: set SANDBOX_GKE_DELETE_CONFIRM='${expected}' to delete the dedicated E2E cluster" >&2
  exit 1
}

gcloud container clusters delete "${cluster}" \
  --project "${project}" \
  --zone "${zone}" \
  --quiet
printf 'Deleted dedicated GKE E2E cluster %s\n' "${cluster}"
