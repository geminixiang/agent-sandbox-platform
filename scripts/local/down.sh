#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

context="colima-${COLIMA_PROFILE}"
delete_profile=false
if [[ "${1:-}" == "--delete-profile" ]]; then delete_profile=true; fi
if [[ "${1:-}" != "" && "${1:-}" != "--delete-profile" ]]; then
  echo "Usage: $0 [--delete-profile]" >&2
  exit 1
fi

if colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "Removing platform test resources..."
  kubectl --context "${context}" delete namespace "${PLATFORM_NAMESPACE}" --ignore-not-found --wait=true
  kubectl --context "${context}" delete -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml" --ignore-not-found --wait=true
  kubectl --context "${context}" delete -f "${REPO_ROOT}/deploy/colima/runtimeclass-gvisor.yaml" --ignore-not-found
fi

if ${delete_profile}; then
  echo "Deleting Colima profile '${COLIMA_PROFILE}'..."
  colima delete --profile "${COLIMA_PROFILE}" --force
else
  cat <<EOF
Cluster resources removed; Colima profile retained.
Use '$0 --delete-profile' to remove the VM and all local cluster data.
EOF
fi
