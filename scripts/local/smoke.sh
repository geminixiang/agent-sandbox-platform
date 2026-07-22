#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

context="colima-${COLIMA_PROFILE}"
if ! colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "ERROR: Colima profile '${COLIMA_PROFILE}' is not running; run ./scripts/local/up.sh" >&2
  exit 1
fi

kubectl --context "${context}" get runtimeclass gvisor >/dev/null
kubectl --context "${context}" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=60s
kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandboxtemplate/platform-gvisor sandboxwarmpool/platform-gvisor >/dev/null

cd "${REPO_ROOT}"
SANDBOX_E2E_KUBECONTEXT="${context}" \
SANDBOX_E2E_NAMESPACE="${PLATFORM_NAMESPACE}" \
npm run test:e2e:kubernetes
