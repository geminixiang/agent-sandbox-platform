#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"
context="colima-${COLIMA_PROFILE}"

"${SCRIPT_DIR}/build-browser.sh"
kubectl --context "${context}" apply -f "${REPO_ROOT}/deploy/colima/browser.yaml"

echo "Waiting for browser WarmPool..."
for _ in $(seq 1 180); do
  ready="$(kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandboxwarmpool platform-browser-gvisor -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready}" == "1" ]] && break
  sleep 1
done
if [[ "${ready:-}" != "1" ]]; then
  kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandbox,pod
  echo "ERROR: browser WarmPool did not become ready" >&2
  exit 1
fi

claim="browser-smoke-$(openssl rand -hex 4)"
cleanup() {
  kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" delete sandboxclaim "${claim}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
}
trap cleanup EXIT

kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: ${claim}
spec:
  warmPoolRef:
    name: platform-browser-gvisor
EOF

for _ in $(seq 1 120); do
  sandbox="$(kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandboxclaim "${claim}" -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"
  [[ -n "${sandbox}" ]] && break
  sleep 1
done
if [[ -z "${sandbox:-}" ]]; then
  echo "ERROR: browser SandboxClaim was not satisfied" >&2
  exit 1
fi
pod="$(kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandbox "${sandbox}" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}' 2>/dev/null || true)"
pod="${pod:-${sandbox}}"
kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" wait --for=condition=Ready "pod/${pod}" --timeout=180s

runtime="$(kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get pod "${pod}" -o jsonpath='{.spec.runtimeClassName}')"
[[ "${runtime}" == "gvisor" ]] || { echo "ERROR: expected gvisor, got '${runtime}'" >&2; exit 1; }

output="$(kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" exec "${pod}" -c browser -- node /opt/browser/smoke.mjs)"
[[ "${output}" == *'"output":"clicked"'* ]] || { echo "ERROR: browser smoke output: ${output}" >&2; exit 1; }
kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" exec "${pod}" -c browser -- test -s /workspace/browser-smoke.png
printf 'Browser gVisor E2E passed: %s\n' "${output}"
