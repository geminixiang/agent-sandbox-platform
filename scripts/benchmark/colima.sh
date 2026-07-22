#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${ROOT}/deploy/colima/versions.env"
context="colima-${COLIMA_PROFILE}"
namespace="${PLATFORM_NAMESPACE}"
port="${SANDBOX_BENCHMARK_PORT:-18791}"
observer_port="${SANDBOX_BENCHMARK_OBSERVER_PORT:-18792}"
samples="${SANDBOX_BENCHMARK_SAMPLES:-10}"
warmups="${SANDBOX_BENCHMARK_WARMUPS:-2}"
concurrency_samples="${SANDBOX_BENCHMARK_CONCURRENCY_SAMPLES:-3}"
stream_samples="${SANDBOX_BENCHMARK_STREAM_SAMPLES:-3}"
output_dir="${SANDBOX_BENCHMARK_OUTPUT_DIR:-${ROOT}/.sandbox-platform/benchmarks}"
tmp="$(mktemp -d)"
control_pid=""
observer_pid=""
cleanup() {
  for pid in "${observer_pid}" "${control_pid}"; do
    [[ -z "${pid}" ]] || kill "${pid}" 2>/dev/null || true
    [[ -z "${pid}" ]] || wait "${pid}" 2>/dev/null || true
  done
  rm -rf "${tmp}"
}
trap cleanup EXIT

"${ROOT}/scripts/local/up.sh"
"${ROOT}/scripts/local/build-browser.sh"
kubectl --context "${context}" apply -f "${ROOT}/deploy/colima/browser.yaml" >/dev/null
for pool in platform-gvisor platform-browser-gvisor; do
  for _ in $(seq 1 180); do
    ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    [[ "${ready}" == 1 ]] && break
    sleep 1
  done
  [[ "${ready:-}" == 1 ]] || { echo "ERROR: ${pool} is not ready" >&2; exit 1; }
done

consumer_id=benchmark-local
consumer_secret="$(openssl rand -hex 32)"
metadata_secret="$(openssl rand -hex 32)"
pools='{"coding":{"warmPoolName":"platform-gvisor","runtimeClassName":"gvisor","containerName":"shell"},"browser":{"warmPoolName":"platform-browser-gvisor","runtimeClassName":"gvisor","containerName":"browser"}}'
(cd "${ROOT}" && go build -o "${tmp}/control-plane" ./apps/control-plane-go/cmd/control-plane)
env \
  SANDBOX_ADDRESS="127.0.0.1:${port}" \
  SANDBOX_K8S_CONTEXT="${context}" \
  SANDBOX_K8S_NAMESPACE="${namespace}" \
  SANDBOX_METADATA_SECRET="${metadata_secret}" \
  SANDBOX_CONSUMER_SECRETS="{\"${consumer_id}\":\"${consumer_secret}\"}" \
  SANDBOX_K8S_POOLS="${pools}" \
  SANDBOX_SWEEP_INTERVAL=1s \
  "${tmp}/control-plane" >"${tmp}/control-plane.log" 2>&1 &
control_pid=$!
for _ in $(seq 1 200); do
  curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  kill -0 "${control_pid}" 2>/dev/null || { cat "${tmp}/control-plane.log" >&2; exit 1; }
  sleep .1
done
curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null

SANDBOX_K8S_CONTEXT="${context}" \
SANDBOX_K8S_NAMESPACE="${namespace}" \
SANDBOX_K8S_POOLS="${pools}" \
SANDBOX_BENCHMARK_OBSERVER_PORT="${observer_port}" \
  uv run python "${ROOT}/tests/benchmark/kubernetes_observer.py" >"${tmp}/observer.log" 2>&1 &
observer_pid=$!
for _ in $(seq 1 100); do
  curl -fsS "http://127.0.0.1:${observer_port}/ready/coding" >/dev/null 2>&1 && break
  kill -0 "${observer_pid}" 2>/dev/null || { cat "${tmp}/observer.log" >&2; exit 1; }
  sleep .1
done

(cd "${ROOT}/packages/sdk-python" && uv build --out-dir "${tmp}/dist" >/dev/null)
uv venv "${tmp}/venv" >/dev/null
uv pip install --python "${tmp}/venv/bin/python" "${tmp}"/dist/*.whl >/dev/null

mkdir -p "${output_dir}"
metadata="${tmp}/metadata.json"
vm_arch="$(colima ssh --profile "${COLIMA_PROFILE}" -- uname -m | tr -d '\r')"
kubernetes_actual="$(kubectl --context "${context}" version -o json | jq -r .serverVersion.gitVersion)"
control_image="$(kubectl --context "${context}" -n "${namespace}" get pod -l agents.x-k8s.io/warm-pool-sandbox -o json | jq -r '.items[] | select(.spec.containers[0].name=="shell") | .status.containerStatuses[0].imageID' | head -1)"
browser_image="$(kubectl --context "${context}" -n "${namespace}" get pod -l agents.x-k8s.io/warm-pool-sandbox -o json | jq -r '.items[] | select(.spec.containers[0].name=="browser") | .status.containerStatuses[0].imageID' | head -1)"
jq -n \
  --arg commit "$(git -C "${ROOT}" rev-parse HEAD)" \
  --arg environment colima-k3s-gvisor \
  --arg architecture "${vm_arch}" \
  --arg kubernetes "${kubernetes_actual}" \
  --arg agentSandbox "${AGENT_SANDBOX_VERSION}" \
  --arg gvisor "${GVISOR_VERSION}" \
  --arg codingImage "${control_image}" \
  --arg browserImage "${browser_image}" \
  --argjson colimaCpu "${COLIMA_CPU}" \
  --argjson colimaMemoryGiB "${COLIMA_MEMORY_GIB}" \
  '{commit:$commit, environment:$environment, architecture:$architecture, kubernetesVersion:$kubernetes, agentSandboxVersion:$agentSandbox, gvisorVersion:$gvisor, codingImage:$codingImage, browserImage:$browserImage, colimaCpu:$colimaCpu, colimaMemoryGiB:$colimaMemoryGiB, warmPoolReplicas:{coding:1,browser:1}}' >"${metadata}"

stamp="$(date -u +%Y-%m-%dT%H%M%SZ)"
json_output="${output_dir}/colima-${stamp}.json"
markdown_output="${output_dir}/colima-${stamp}.md"
cd "${ROOT}"
SANDBOX_PLATFORM_URL="http://127.0.0.1:${port}" \
SANDBOX_TEST_CONSUMER_ID="${consumer_id}" \
SANDBOX_TEST_CONSUMER_SECRET="${consumer_secret}" \
SANDBOX_BENCHMARK_OBSERVER_URL="http://127.0.0.1:${observer_port}" \
SANDBOX_BENCHMARK_METADATA="${metadata}" \
SANDBOX_BENCHMARK_JSON="${json_output}" \
SANDBOX_BENCHMARK_MARKDOWN="${markdown_output}" \
SANDBOX_BENCHMARK_SAMPLES="${samples}" \
SANDBOX_BENCHMARK_WARMUPS="${warmups}" \
SANDBOX_BENCHMARK_CONCURRENCY_SAMPLES="${concurrency_samples}" \
SANDBOX_BENCHMARK_STREAM_SAMPLES="${stream_samples}" \
  "${tmp}/venv/bin/python" -m tests.benchmark.colima_baseline

printf 'Raw benchmark: %s\nMarkdown report: %s\n' "${json_output}" "${markdown_output}"
