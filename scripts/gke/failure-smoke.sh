#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
credentials="${SANDBOX_GKE_CREDENTIALS:-${ROOT}/.sandbox-platform/gke-e2e.json}"
[[ -f "${credentials}" ]] || { echo "ERROR: missing ${credentials}" >&2; exit 1; }
ctx="$(jq -er .context "${credentials}")"
namespace="$(jq -er .namespace "${credentials}")"
consumer_id="$(jq -er .consumerId "${credentials}")"
consumer_secret="$(jq -er .consumerSecret "${credentials}")"
[[ "${ctx}" == gke_glab-384109_*_agent-sandbox-e2e ]] || { echo "ERROR: refusing unexpected context ${ctx}" >&2; exit 1; }

port="${SANDBOX_GKE_FAILURE_PORT:-18790}"
tmp="$(mktemp -d)"
pf=""
restore() {
  kubectl --context "${ctx}" -n "${namespace}" patch sandboxwarmpool platform-coding --type merge -p '{"spec":{"replicas":1}}' >/dev/null 2>&1 || true
  kubectl --context "${ctx}" -n "${namespace}" patch sandboxwarmpool platform-browser --type merge -p '{"spec":{"replicas":1}}' >/dev/null 2>&1 || true
  if [[ -n "${pf}" ]]; then
    kill "${pf}" 2>/dev/null || true
    wait "${pf}" 2>/dev/null || true
  fi
  rm -rf "${tmp}"
}
trap restore EXIT

kubectl --context "${ctx}" -n "${namespace}" port-forward service/agent-sandbox-platform-agent-sandbox-platform "${port}:8787" >"${tmp}/port-forward.log" 2>&1 &
pf=$!
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null

(cd "${ROOT}/packages/sdk-python" && uv build --out-dir "${tmp}/dist" >/dev/null)
uv venv "${tmp}/venv" >/dev/null
uv pip install --python "${tmp}/venv/bin/python" "${tmp}"/dist/*.whl >/dev/null
SANDBOX_PLATFORM_URL="http://127.0.0.1:${port}" \
SANDBOX_TEST_CONSUMER_ID="${consumer_id}" \
SANDBOX_TEST_CONSUMER_SECRET="${consumer_secret}" \
  "${tmp}/venv/bin/python" "${ROOT}/tests/e2e/gke-failure-recovery.py"

kubectl --context "${ctx}" -n "${namespace}" patch sandboxwarmpool platform-browser --type merge -p '{"spec":{"replicas":0}}' >/dev/null
for _ in $(seq 1 90); do
  desired="$(kubectl --context "${ctx}" -n "${namespace}" get sandboxwarmpool platform-browser -o jsonpath='{.spec.replicas}')"
  ready="$(kubectl --context "${ctx}" -n "${namespace}" get sandboxwarmpool platform-browser -o jsonpath='{.status.readyReplicas}')"
  [[ "${desired}" == 0 && "${ready}" == 0 ]] && break
  sleep 1
done
status="$(curl -sS -o "${tmp}/not-ready.json" -w '%{http_code}' "http://127.0.0.1:${port}/ready")"
[[ "${status}" == 503 ]] || { echo "ERROR: readiness remained HTTP ${status} with zero browser capacity" >&2; exit 1; }

kubectl --context "${ctx}" -n "${namespace}" patch sandboxwarmpool platform-browser --type merge -p '{"spec":{"replicas":1}}' >/dev/null
for _ in $(seq 1 120); do
  ready="$(kubectl --context "${ctx}" -n "${namespace}" get sandboxwarmpool platform-browser -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready}" == 1 ]] && curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  sleep 1
done
[[ "${ready:-}" == 1 ]] || { echo "ERROR: browser capacity did not recover" >&2; exit 1; }
curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null
printf 'GKE failure and recovery E2E passed\n'
