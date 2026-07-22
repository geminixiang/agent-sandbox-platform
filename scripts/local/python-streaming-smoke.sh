#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

context="colima-${COLIMA_PROFILE}"
namespace="${PLATFORM_NAMESPACE}"
port="${SANDBOX_PYTHON_STREAMING_PORT:-18789}"
consumer_id="python-streaming-e2e"
subject_id="streaming-owner"
other_subject_id="streaming-other"

if ! colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "ERROR: Colima profile '${COLIMA_PROFILE}' is not running; run ./scripts/local/up.sh" >&2
  exit 1
fi
kubectl --context "${context}" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=60s

consumer_secret="$(openssl rand -hex 32)"
metadata_secret="$(openssl rand -hex 32)"
consumer_hash="$(printf '["consumer","%s"]' "${consumer_id}" | openssl dgst -sha256 -hmac "${metadata_secret}" | awk '{print $NF}' | cut -c1-40)"
temp_dir="$(mktemp -d)"
binary="${temp_dir}/control-plane"
log="${temp_dir}/control-plane.log"
pid=""

list_claims() {
  kubectl --context "${context}" -n "${namespace}" get sandboxclaims \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | sort
}

cleanup() {
  status=$?
  if [[ ${status} -ne 0 && -f "${log}" ]]; then
    echo "--- control-plane log ---" >&2
    cat "${log}" >&2
  fi
  if [[ -n "${pid}" ]]; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
  kubectl --context "${context}" -n "${namespace}" delete sandboxclaims \
    -l "sandbox.geminixiang.dev/consumer=${consumer_hash}" --ignore-not-found --wait=true \
    >/dev/null 2>&1 || true
  rm -rf "${temp_dir}"
  return "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

list_claims >"${temp_dir}/claims-before"
"${SCRIPT_DIR}/build-coding.sh"
"${SCRIPT_DIR}/build-browser.sh"
kubectl --context "${context}" apply -f "${REPO_ROOT}/deploy/colima/e2e.yaml" >/dev/null
kubectl --context "${context}" apply -f "${REPO_ROOT}/deploy/colima/browser.yaml" >/dev/null

for pool in platform-gvisor platform-browser-gvisor; do
  ready=""
  for _ in $(seq 1 180); do
    ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    [[ "${ready}" == "1" ]] && break
    sleep 1
  done
  [[ "${ready}" == "1" ]] || { echo "ERROR: WarmPool ${pool} did not become ready" >&2; exit 1; }
done

(cd "${REPO_ROOT}" && go build -o "${binary}" ./apps/control-plane-go/cmd/control-plane)
pools='{"coding":{"warmPoolName":"platform-gvisor","runtimeClassName":"gvisor","containerName":"shell"},"browser":{"warmPoolName":"platform-browser-gvisor","runtimeClassName":"gvisor","containerName":"browser"}}'
env \
  SANDBOX_ADDRESS="127.0.0.1:${port}" \
  SANDBOX_K8S_CONTEXT="${context}" \
  SANDBOX_K8S_NAMESPACE="${namespace}" \
  SANDBOX_METADATA_SECRET="${metadata_secret}" \
  SANDBOX_CONSUMER_SECRETS="{\"${consumer_id}\":\"${consumer_secret}\"}" \
  SANDBOX_K8S_POOLS="${pools}" \
  SANDBOX_FILE_TRANSFER_MAX_CONCURRENT=2 \
  SANDBOX_FILE_TRANSFER_MAX_PER_LEASE=1 \
  SANDBOX_FILE_TRANSFER_TIMEOUT=3m \
  SANDBOX_SWEEP_INTERVAL=1s \
  "${binary}" >"${log}" 2>&1 &
pid="$!"
for _ in $(seq 1 200); do
  curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  kill -0 "${pid}" 2>/dev/null || { cat "${log}" >&2; exit 1; }
  sleep .1
done
curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null

(cd "${REPO_ROOT}/packages/sdk-python" && uv build --out-dir "${temp_dir}/dist" >/dev/null)
uv venv "${temp_dir}/venv" >/dev/null
uv pip install --python "${temp_dir}/venv/bin/python" "${temp_dir}"/dist/*.whl >/dev/null
mkdir "${temp_dir}/run"
(
  cd "${temp_dir}/run"
  SANDBOX_PLATFORM_URL="http://127.0.0.1:${port}" \
  SANDBOX_TEST_CONSUMER_ID="${consumer_id}" \
  SANDBOX_TEST_SUBJECT_ID="${subject_id}" \
  SANDBOX_TEST_OTHER_SUBJECT_ID="${other_subject_id}" \
  SANDBOX_TEST_CONSUMER_SECRET="${consumer_secret}" \
    "${temp_dir}/venv/bin/python" "${REPO_ROOT}/tests/e2e/python-streaming.py" >"${temp_dir}/result.json"
)
jq -e '.status == "passed" and (.streaming | length == 2)' "${temp_dir}/result.json" >/dev/null

for _ in $(seq 1 60); do
  list_claims >"${temp_dir}/claims-after"
  cmp -s "${temp_dir}/claims-before" "${temp_dir}/claims-after" && break
  sleep 1
done
cmp -s "${temp_dir}/claims-before" "${temp_dir}/claims-after" || {
  echo "ERROR: SandboxClaims were not restored to their pre-test state" >&2
  diff -u "${temp_dir}/claims-before" "${temp_dir}/claims-after" >&2 || true
  exit 1
}
for pool in platform-gvisor platform-browser-gvisor; do
  ready=""
  for _ in $(seq 1 180); do
    ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    [[ "${ready}" == "1" ]] && break
    sleep 1
  done
  [[ "${ready}" == "1" ]] || { echo "ERROR: WarmPool ${pool} did not recover" >&2; exit 1; }
done
coding_ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool platform-gvisor -o jsonpath='{.status.readyReplicas}')"
browser_ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool platform-browser-gvisor -o jsonpath='{.status.readyReplicas}')"

jq -Rn '[inputs | select(length > 0)]' <"${temp_dir}/claims-before" >"${temp_dir}/claims-before.json"
jq -Rn '[inputs | select(length > 0)]' <"${temp_dir}/claims-after" >"${temp_dir}/claims-after.json"
coding_image="$(kubectl --context "${context}" -n "${namespace}" get pods -l agents.x-k8s.io/warm-pool-sandbox -o json | jq -r '.items[] | select(.spec.containers[0].name=="shell") | .status.containerStatuses[0].imageID' | head -1)"
browser_image="$(kubectl --context "${context}" -n "${namespace}" get pods -l agents.x-k8s.io/warm-pool-sandbox -o json | jq -r '.items[] | select(.spec.containers[0].name=="browser") | .status.containerStatuses[0].imageID' | head -1)"
commit="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
report_dir="${REPO_ROOT}/.sandbox-platform/test-reports"
mkdir -p "${report_dir}"
report="${report_dir}/python-streaming-$(date -u +%Y-%m-%dT%H%M%SZ).json"
jq -n \
  --arg commit "${commit}" \
  --arg context "${context}" \
  --arg namespace "${namespace}" \
  --arg codingImageID "${coding_image}" \
  --arg browserImageID "${browser_image}" \
  --argjson codingReady "${coding_ready}" \
  --argjson browserReady "${browser_ready}" \
  --slurpfile before "${temp_dir}/claims-before.json" \
  --slurpfile after "${temp_dir}/claims-after.json" \
  --slurpfile result "${temp_dir}/result.json" \
  '{schemaVersion:1,test:"python-streaming-smoke",status:"passed",gitCommit:$commit,kubernetes:{context:$context,namespace:$namespace,warmPools:{coding:{name:"platform-gvisor",readyReplicas:$codingReady,imageID:$codingImageID},browser:{name:"platform-browser-gvisor",readyReplicas:$browserReady,imageID:$browserImageID}},claimsBefore:$before[0],claimsAfter:$after[0]},result:$result[0]}' \
  >"${report}"

jq -e '
  .status == "passed" and
  (.gitCommit | test("^[0-9a-f]{40}$")) and
  (.kubernetes.warmPools.coding.readyReplicas == 1) and
  (.kubernetes.warmPools.browser.readyReplicas == 1) and
  (.kubernetes.warmPools.coding.imageID | length > 0) and
  (.kubernetes.warmPools.browser.imageID | length > 0) and
  (.kubernetes.claimsBefore == .kubernetes.claimsAfter)
' "${report}" >/dev/null
if grep -Fq "${consumer_secret}" "${report}" || grep -Fq "${metadata_secret}" "${report}"; then
  echo "ERROR: evidence report contains a secret" >&2
  rm -f "${report}"
  exit 1
fi

printf 'Built-wheel Python streaming E2E passed\nEvidence: %s\n' "${report}"
