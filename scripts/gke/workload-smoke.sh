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

port="${SANDBOX_GKE_E2E_PORT:-18789}"
tmp="$(mktemp -d)"
pf=""
policy_applied=false
cleanup() {
  if [[ -n "${pf}" ]]; then
    kill "${pf}" 2>/dev/null || true
    wait "${pf}" 2>/dev/null || true
  fi
  if [[ "${policy_applied}" == true ]]; then
    helm upgrade agent-sandbox-platform "${ROOT}/deploy/helm" \
      --kube-context "${ctx}" --namespace "${namespace}" \
      --values "${ROOT}/deploy/gke/values-gvisor.yaml" \
      --wait --timeout 10m >/dev/null 2>&1 || true
  fi
  kubectl --context "${ctx}" delete -f "${ROOT}/tests/fixtures/workload-site.yaml" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "${tmp}"
}
trap cleanup EXIT
kubectl --context "${ctx}" -n "${namespace}" get sandboxclaims -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort >"${tmp}/claims-before"

kubectl --context "${ctx}" apply -f "${ROOT}/tests/fixtures/workload-site.yaml" >/dev/null
kubectl --context "${ctx}" -n "${namespace}" rollout status deployment/workload-fixture --timeout=3m
helm upgrade agent-sandbox-platform "${ROOT}/deploy/helm" \
  --kube-context "${ctx}" --namespace "${namespace}" \
  --values "${ROOT}/deploy/gke/values-gvisor.yaml" \
  --values "${ROOT}/deploy/gke/values-workload-e2e.yaml" \
  --wait --timeout 10m >/dev/null
policy_applied=true
fixture_url="http://workload-fixture.${namespace}.svc.cluster.local:8080"
kubectl --context "${ctx}" -n "${namespace}" port-forward service/agent-sandbox-platform-agent-sandbox-platform "${port}:8787" >"${tmp}/port-forward.log" 2>&1 &
pf=$!
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  kill -0 "${pf}" 2>/dev/null || { cat "${tmp}/port-forward.log" >&2; exit 1; }
  sleep 1
done
curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null

(cd "${ROOT}/packages/sdk-python" && uv build --out-dir "${tmp}/dist" >/dev/null)
uv venv "${tmp}/venv" >/dev/null
uv pip install --python "${tmp}/venv/bin/python" "${tmp}"/dist/*.whl >/dev/null
SANDBOX_PLATFORM_URL="http://127.0.0.1:${port}" \
SANDBOX_FIXTURE_URL="${fixture_url}" \
SANDBOX_TEST_CONSUMER_ID="${consumer_id}" \
SANDBOX_TEST_CONSUMER_SECRET="${consumer_secret}" \
  "${tmp}/venv/bin/python" "${ROOT}/tests/e2e/gke-daily-work.py" | tee "${tmp}/result.json"

for _ in $(seq 1 60); do
  kubectl --context "${ctx}" -n "${namespace}" get sandboxclaims -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort >"${tmp}/claims-after"
  comm -13 "${tmp}/claims-before" "${tmp}/claims-after" >"${tmp}/new-claims"
  [[ ! -s "${tmp}/new-claims" ]] && break
  sleep 1
done
[[ ! -s "${tmp}/new-claims" ]] || { echo "ERROR: test Claims remained after release: $(tr '\n' ' ' <"${tmp}/new-claims")" >&2; exit 1; }

kubectl --context "${ctx}" -n "${namespace}" get sandboxwarmpools
for pod in $(kubectl --context "${ctx}" -n "${namespace}" get pod -l agents.x-k8s.io/warm-pool-sandbox -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  runtime="$(kubectl --context "${ctx}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.spec.runtimeClassName}')"
  node="$(kubectl --context "${ctx}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.spec.nodeName}')"
  node_runtime="$(kubectl --context "${ctx}" get node "${node}" -o jsonpath='{.metadata.labels.sandbox\.gke\.io/runtime}')"
  [[ "${runtime}" == gvisor && "${node_runtime}" == gvisor ]] || { echo "ERROR: ${pod} is not on GKE gVisor" >&2; exit 1; }
done
printf 'GKE daily-work E2E passed\n'
