#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

"${SCRIPT_DIR}/up.sh"
"${SCRIPT_DIR}/build-browser.sh"
kubectl --context "colima-${COLIMA_PROFILE}" apply -f "${REPO_ROOT}/deploy/colima/browser.yaml"
for _ in $(seq 1 120); do
  ready="$(kubectl --context "colima-${COLIMA_PROFILE}" -n "${PLATFORM_NAMESPACE}" get sandboxwarmpool platform-browser-gvisor -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready}" == "1" ]] && break
  sleep 1
done
[[ "${ready:-}" == "1" ]] || { echo "ERROR: browser WarmPool did not become ready" >&2; exit 1; }

port="${SANDBOX_PYTHON_E2E_PORT:-18788}"
consumer_id=python-e2e
subject_id=python-browser
consumer_secret="$(openssl rand -hex 32)"
metadata_secret="$(openssl rand -hex 32)"
temp_dir="$(mktemp -d)"
binary="${temp_dir}/control-plane"
log="${temp_dir}/control-plane.log"
cleanup() {
  kill "${pid:-}" 2>/dev/null || true
  wait "${pid:-}" 2>/dev/null || true
  rm -rf "${temp_dir}"
}
trap cleanup EXIT

(cd "${REPO_ROOT}" && go build -o "${binary}" ./apps/control-plane-go/cmd/control-plane)
env \
  SANDBOX_ADDRESS="127.0.0.1:${port}" \
  SANDBOX_K8S_CONTEXT="colima-${COLIMA_PROFILE}" \
  SANDBOX_K8S_NAMESPACE="${PLATFORM_NAMESPACE}" \
  SANDBOX_METADATA_SECRET="${metadata_secret}" \
  SANDBOX_CONSUMER_SECRETS="{\"${consumer_id}\":\"${consumer_secret}\"}" \
  SANDBOX_K8S_POOLS='{"browser":{"warmPoolName":"platform-browser-gvisor","runtimeClassName":"gvisor","containerName":"browser"}}' \
  SANDBOX_SWEEP_INTERVAL=1s \
  "${binary}" >"${log}" 2>&1 &
pid="$!"
for _ in $(seq 1 200); do
  curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  kill -0 "${pid}" 2>/dev/null || { cat "${log}" >&2; exit 1; }
  sleep .1
done

cd "${REPO_ROOT}/packages/sdk-python"
rm -rf dist
uv build >/dev/null
uv venv "${temp_dir}/venv" >/dev/null
uv pip install --python "${temp_dir}/venv/bin/python" dist/*.whl >/dev/null
SANDBOX_PLATFORM_URL="http://127.0.0.1:${port}" \
SANDBOX_TEST_CONSUMER_ID="${consumer_id}" \
SANDBOX_TEST_SUBJECT_ID="${subject_id}" \
SANDBOX_TEST_CONSUMER_SECRET="${consumer_secret}" \
"${temp_dir}/venv/bin/python" "${REPO_ROOT}/tests/e2e/python-browser.py"

echo "Built-wheel Python browser E2E passed"
