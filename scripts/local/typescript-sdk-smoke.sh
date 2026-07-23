#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

context="colima-${COLIMA_PROFILE}"
namespace="${PLATFORM_NAMESPACE}"
consumer_id="typescript-sdk-e2e-$$-${RANDOM}"
owner_subject_id="typescript-owner"
other_subject_id="typescript-other"
report_dir="${REPO_ROOT}/.sandbox-platform/test-reports"
temp_dir="$(mktemp -d)"
binary="${temp_dir}/control-plane"
log="${temp_dir}/control-plane.log"
pid=""
consumer_secret=""
metadata_secret=""
consumer_hash=""
owner_token_a=""
owner_token_b=""
other_token=""

required_commands=(colima kubectl jq curl openssl go node npm)
for command in "${required_commands[@]}"; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "ERROR: missing required command '${command}'" >&2
    exit 1
  }
done

if ! colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "ERROR: Colima profile '${COLIMA_PROFILE}' is not running; run ./scripts/local/up.sh" >&2
  exit 1
fi
kubectl --context "${context}" -n agent-sandbox-system rollout status \
  deployment/agent-sandbox-controller --timeout=60s

list_claims() {
  if ! kubectl --context "${context}" get namespace "${namespace}" >/dev/null 2>&1; then
    return 0
  fi
  kubectl --context "${context}" -n "${namespace}" get sandboxclaims \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | sort
}

expected_image_id() {
  local image="$1"
  colima ssh --profile "${COLIMA_PROFILE}" -- \
    sudo k3s crictl inspecti "${image}" | jq -r '.status.id'
}

warm_pool_image_id() {
  local container="$1"
  kubectl --context "${context}" -n "${namespace}" get pods \
    -l agents.x-k8s.io/warm-pool-sandbox -o json 2>/dev/null |
    jq -r --arg container "${container}" \
      '.items[] | select(.metadata.deletionTimestamp == null) | select(.spec.containers[0].name == $container) | .status.containerStatuses[0].imageID // empty' |
    head -1
}

apply_if_needed() {
  local manifest="$1" rc
  set +e
  kubectl --context "${context}" diff -f "${manifest}" >/dev/null 2>&1
  rc=$?
  set -e
  case "${rc}" in
    0) echo "Deployment manifest $(basename "${manifest}") is already current" ;;
    1) kubectl --context "${context}" apply -f "${manifest}" >/dev/null ;;
    *) echo "ERROR: could not diff ${manifest}" >&2; return "${rc}" ;;
  esac
}

converge_warm_pool_image() {
  local pool="$1" container="$2" expected="$3" ready="" actual=""
  for _ in $(seq 1 20); do
    ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    actual="$(warm_pool_image_id "${container}")"
    if [[ "${ready}" == "1" && "${actual}" == "${expected}" ]]; then
      echo "WarmPool ${pool} already uses current image ${expected}"
      return 0
    fi
    sleep 1
  done

  echo "Recycling WarmPool ${pool} to pick up current image ${expected}"
  kubectl --context "${context}" -n "${namespace}" patch sandboxwarmpool "${pool}" \
    --type=merge -p '{"spec":{"replicas":0}}' >/dev/null
  for _ in $(seq 1 180); do
    [[ "$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" \
      -o jsonpath='{.status.replicas}' 2>/dev/null || true)" == "0" ]] && break
    sleep 1
  done
  kubectl --context "${context}" -n "${namespace}" patch sandboxwarmpool "${pool}" \
    --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
  for _ in $(seq 1 240); do
    ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    actual="$(warm_pool_image_id "${container}")"
    [[ "${ready}" == "1" && "${actual}" == "${expected}" ]] && return 0
    sleep 1
  done
  echo "ERROR: WarmPool ${pool} did not run expected image ${expected}; got ${actual:-none}" >&2
  return 1
}

mint_subject_token() {
  local subject_id="$1" ttl_seconds="$2" expires_at claims payload signed signature
  expires_at="$(( $(date +%s) + ttl_seconds ))"
  claims="$(jq -cn \
    --arg consumerId "${consumer_id}" \
    --arg subjectId "${subject_id}" \
    --argjson exp "${expires_at}" \
    '{consumerId:$consumerId,subjectId:$subjectId,exp:$exp}')"
  payload="$(printf '%s' "${claims}" | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
  signed="v1.${payload}"
  signature="$(printf '%s' "${signed}" | openssl dgst -sha256 -hmac "${consumer_secret}" -binary |
    openssl base64 -A | tr '+/' '-_' | tr -d '=')"
  printf '%s.%s' "${signed}" "${signature}"
}

cleanup() {
  local status=$? failure_stamp="" failure_log=""
  if [[ -n "${pid}" ]]; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
  if [[ -n "${consumer_hash}" ]]; then
    kubectl --context "${context}" -n "${namespace}" delete sandboxclaims \
      -l "sandbox.geminixiang.dev/consumer=${consumer_hash}" \
      --ignore-not-found --wait=true >/dev/null 2>&1 || true
  fi
  if [[ ${status} -ne 0 ]]; then
    mkdir -p "${report_dir}"
    failure_stamp="$(date -u +%Y-%m-%dT%H%M%SZ)"
    if [[ -f "${log}" ]]; then
      failure_log="${report_dir}/typescript-sdk-failure-${failure_stamp}-control-plane.log"
      cp "${log}" "${failure_log}"
      chmod 600 "${failure_log}"
      printf '%s\n' "ERROR: preserved control-plane log: ${failure_log}" >&2
    fi
    if [[ -f "${temp_dir}/consumer.log" ]]; then
      failure_log="${report_dir}/typescript-sdk-failure-${failure_stamp}-consumer.log"
      cp "${temp_dir}/consumer.log" "${failure_log}"
      chmod 600 "${failure_log}"
      printf '%s\n' "ERROR: preserved consumer log: ${failure_log}" >&2
    fi
  fi
  rm -rf "${temp_dir}"
  return "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

list_claims >"${temp_dir}/claims-before"
"${SCRIPT_DIR}/build-coding.sh"
"${SCRIPT_DIR}/build-browser.sh"
apply_if_needed "${REPO_ROOT}/deploy/colima/e2e.yaml"
apply_if_needed "${REPO_ROOT}/deploy/colima/browser.yaml"

expected_coding_image="$(expected_image_id agent-sandbox-coding:local)"
expected_browser_image="$(expected_image_id agent-sandbox-browser:local)"
[[ -n "${expected_coding_image}" && -n "${expected_browser_image}" ]] || {
  echo "ERROR: a current runtime image is missing" >&2
  exit 1
}
converge_warm_pool_image platform-gvisor shell "${expected_coding_image}"
converge_warm_pool_image platform-browser-gvisor browser "${expected_browser_image}"

consumer_secret="$(openssl rand -hex 32)"
metadata_secret="$(openssl rand -hex 32)"
consumer_hash="$(printf '["consumer","%s"]' "${consumer_id}" |
  openssl dgst -sha256 -hmac "${metadata_secret}" | awk '{print $NF}' | cut -c1-40)"
port="${SANDBOX_TYPESCRIPT_SDK_PORT:-$(node --input-type=module --eval '
  import net from "node:net";
  const server = net.createServer();
  server.listen(0, "127.0.0.1", () => {
    console.log(server.address().port);
    server.close();
  });
')}"

(cd "${REPO_ROOT}" && go build -o "${binary}" ./apps/control-plane-go/cmd/control-plane)
pools='{"coding":{"warmPoolName":"platform-gvisor","runtimeClassName":"gvisor","containerName":"shell"},"browser":{"warmPoolName":"platform-browser-gvisor","runtimeClassName":"gvisor","containerName":"browser"}}'
env \
  SANDBOX_ADDRESS="127.0.0.1:${port}" \
  SANDBOX_K8S_CONTEXT="${context}" \
  SANDBOX_K8S_NAMESPACE="${namespace}" \
  SANDBOX_METADATA_SECRET="${metadata_secret}" \
  SANDBOX_CONSUMER_SECRETS="{\"${consumer_id}\":\"${consumer_secret}\"}" \
  SANDBOX_K8S_POOLS="${pools}" \
  SANDBOX_FILE_TRANSFER_MAX_CONCURRENT=4 \
  SANDBOX_FILE_TRANSFER_MAX_PER_LEASE=2 \
  SANDBOX_FILE_TRANSFER_TIMEOUT=3m \
  SANDBOX_SWEEP_INTERVAL=1s \
  "${binary}" >"${log}" 2>&1 &
pid="$!"
for _ in $(seq 1 200); do
  curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null 2>&1 && break
  kill -0 "${pid}" 2>/dev/null || {
    echo "ERROR: temporary control plane exited before becoming ready" >&2
    exit 1
  }
  sleep .1
done
curl -fsS "http://127.0.0.1:${port}/ready" >/dev/null

mkdir -p "${temp_dir}/dist" "${temp_dir}/consumer" "${temp_dir}/home"
pack_json="$(cd "${REPO_ROOT}/packages/sdk-typescript" &&
  npm pack --ignore-scripts --json --pack-destination "${temp_dir}/dist")"
tarball_name="$(jq -er '.[0].filename' <<<"${pack_json}")"
tarball="${temp_dir}/dist/${tarball_name}"
package_version="$(jq -er '.[0].version' <<<"${pack_json}")"
[[ "${package_version}" == "0.2.0-rc.1" ]] || {
  echo "ERROR: expected package 0.2.0-rc.1, got ${package_version}" >&2
  exit 1
}
printf '{"private":true,"type":"module"}\n' >"${temp_dir}/consumer/package.json"
(
  cd "${temp_dir}/consumer"
  npm install --ignore-scripts --no-audit --no-fund --package-lock=false "${tarball}" >/dev/null
)
[[ ! -e "${temp_dir}/consumer/package-lock.json" ]] || {
  echo "ERROR: clean acceptance install generated a package lock" >&2
  exit 1
}
installed_version="$(cd "${temp_dir}/consumer" && \
  npm list --json @geminixiang/sandbox-sdk | jq -er '.dependencies["@geminixiang/sandbox-sdk"].version')"
[[ "${installed_version}" == "${package_version}" ]] || {
  echo "ERROR: installed package version ${installed_version} does not match ${package_version}" >&2
  exit 1
}
cp "${REPO_ROOT}/tests/e2e/typescript-sdk.mjs" "${temp_dir}/consumer/typescript-sdk.mjs"

# Mint only after build/install so every token remains short-lived for the real SDK operations.
owner_token_a="$(mint_subject_token "${owner_subject_id}" 299)"
owner_token_b="$(mint_subject_token "${owner_subject_id}" 300)"
other_token="$(mint_subject_token "${other_subject_id}" 300)"
[[ "${owner_token_a}" != "${owner_token_b}" ]] || {
  echo "ERROR: rotating provider tokens must be distinct" >&2
  exit 1
}
(
  cd "${temp_dir}/consumer"
  env -i \
    HOME="${temp_dir}/home" \
    PATH="${PATH}" \
    SANDBOX_PLATFORM_URL="http://127.0.0.1:${port}" \
    SANDBOX_TEST_OWNER_TOKEN_A="${owner_token_a}" \
    SANDBOX_TEST_OWNER_TOKEN_B="${owner_token_b}" \
    SANDBOX_TEST_OTHER_TOKEN="${other_token}" \
    node ./typescript-sdk.mjs >"${temp_dir}/result.json" 2>"${temp_dir}/consumer.log"
)
jq -e '
  .status == "passed" and
  (.coverage.durationMs > 0 and .coverage.durationMs <= 600000) and
  .coverage.rotatingAsyncTokenProvider.tokenSlots == 2 and
  (.coverage.streaming | length == 2) and
  ([.coverage.streaming[].bytes] | all(. == 10485760))
' "${temp_dir}/result.json" >/dev/null

for _ in $(seq 1 90); do
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
    ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    [[ "${ready}" == "1" ]] && break
    sleep 1
  done
  [[ "${ready}" == "1" ]] || {
    echo "ERROR: WarmPool ${pool} did not recover" >&2
    exit 1
  }
done

coding_image="$(warm_pool_image_id shell)"
browser_image="$(warm_pool_image_id browser)"
[[ "${coding_image}" == "${expected_coding_image}" && "${browser_image}" == "${expected_browser_image}" ]] || {
  echo "ERROR: accepted Pods do not match the images built for this run" >&2
  exit 1
}
coding_runtime="$(kubectl --context "${context}" -n "${namespace}" get pods \
  -l agents.x-k8s.io/warm-pool-sandbox -o json |
  jq -r '.items[] | select(.metadata.deletionTimestamp == null) | select(.spec.containers[0].name == "shell") | .spec.runtimeClassName' | head -1)"
browser_runtime="$(kubectl --context "${context}" -n "${namespace}" get pods \
  -l agents.x-k8s.io/warm-pool-sandbox -o json |
  jq -r '.items[] | select(.metadata.deletionTimestamp == null) | select(.spec.containers[0].name == "browser") | .spec.runtimeClassName' | head -1)"
[[ "${coding_runtime}" == "gvisor" && "${browser_runtime}" == "gvisor" ]] || {
  echo "ERROR: accepted Pods are not using the gvisor RuntimeClass" >&2
  exit 1
}

gvisor_actual_version="$(colima ssh --profile "${COLIMA_PROFILE}" -- \
  sh -lc 'runsc --version 2>/dev/null | head -1' | tr -d '\r')"
[[ "${gvisor_actual_version}" == *"release-${GVISOR_VERSION}"* ]] || {
  echo "ERROR: actual gVisor version does not match ${GVISOR_VERSION}" >&2
  exit 1
}
agent_sandbox_controller_image="$(kubectl --context "${context}" -n agent-sandbox-system get \
  deployment agent-sandbox-controller -o jsonpath='{.spec.template.spec.containers[0].image}')"
[[ "${agent_sandbox_controller_image}" == *":${AGENT_SANDBOX_VERSION}" ]] || {
  echo "ERROR: Agent Sandbox controller does not match ${AGENT_SANDBOX_VERSION}" >&2
  exit 1
}
kubernetes_version="$(kubectl --context "${context}" version -o json | jq -c '{clientVersion,serverVersion}')"
kubernetes_server_version="$(jq -r '.serverVersion.gitVersion' <<<"${kubernetes_version}")"
[[ "${kubernetes_server_version}" == "${KUBERNETES_VERSION}" ]] || {
  echo "ERROR: k3s server ${kubernetes_server_version} does not match ${KUBERNETES_VERSION}" >&2
  exit 1
}
node_version="$(node --version)"
commit="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
coding_ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool \
  platform-gvisor -o jsonpath='{.status.readyReplicas}')"
browser_ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool \
  platform-browser-gvisor -o jsonpath='{.status.readyReplicas}')"
jq -Rn '[inputs | select(length > 0)]' <"${temp_dir}/claims-before" >"${temp_dir}/claims-before.json"
jq -Rn '[inputs | select(length > 0)]' <"${temp_dir}/claims-after" >"${temp_dir}/claims-after.json"

mkdir -p "${report_dir}"
report="${report_dir}/typescript-sdk-$(date -u +%Y-%m-%dT%H%M%SZ).json"
jq -n \
  --arg commit "${commit}" \
  --arg packageVersion "${package_version}" \
  --arg nodeVersion "${node_version}" \
  --arg context "${context}" \
  --arg namespace "${namespace}" \
  --arg kubernetesVersionPin "${KUBERNETES_VERSION}" \
  --argjson kubernetesVersion "${kubernetes_version}" \
  --arg gvisorVersionPin "${GVISOR_VERSION}" \
  --arg gvisorVersionActual "${gvisor_actual_version}" \
  --arg agentSandboxVersion "${AGENT_SANDBOX_VERSION}" \
  --arg agentSandboxControllerImage "${agent_sandbox_controller_image}" \
  --arg codingImageID "${coding_image}" \
  --arg browserImageID "${browser_image}" \
  --argjson codingReady "${coding_ready}" \
  --argjson browserReady "${browser_ready}" \
  --slurpfile before "${temp_dir}/claims-before.json" \
  --slurpfile after "${temp_dir}/claims-after.json" \
  --slurpfile result "${temp_dir}/result.json" \
  '{schemaVersion:1,test:"typescript-sdk-smoke",status:"passed",gitCommit:$commit,package:{name:"@geminixiang/sandbox-sdk",version:$packageVersion},node:$nodeVersion,kubernetes:{context:$context,namespace:$namespace,versionPin:$kubernetesVersionPin,version:$kubernetesVersion,gvisor:{pin:$gvisorVersionPin,actual:$gvisorVersionActual},agentSandbox:{version:$agentSandboxVersion,controllerImage:$agentSandboxControllerImage},warmPools:{coding:{name:"platform-gvisor",readyReplicas:$codingReady,runtimeClass:"gvisor",imageID:$codingImageID},browser:{name:"platform-browser-gvisor",readyReplicas:$browserReady,runtimeClass:"gvisor",imageID:$browserImageID}},claimsBefore:$before[0],claimsAfter:$after[0]},coverage:$result[0].coverage}' \
  >"${report}"

jq -e '
  .status == "passed" and
  (.gitCommit | test("^[0-9a-f]{40}$")) and
  .package.version == "0.2.0-rc.1" and
  .kubernetes.warmPools.coding.readyReplicas == 1 and
  .kubernetes.warmPools.browser.readyReplicas == 1 and
  (.kubernetes.warmPools.coding.imageID | length > 0) and
  (.kubernetes.warmPools.browser.imageID | length > 0) and
  (.kubernetes.claimsBefore == .kubernetes.claimsAfter) and
  (.coverage.durationMs > 0 and .coverage.durationMs <= 600000) and
  (.coverage.streaming | length == 2)
' "${report}" >/dev/null
for sensitive in \
  "${consumer_secret}" "${metadata_secret}" \
  "${owner_token_a}" "${owner_token_b}" "${other_token}"; do
  if grep -Fq "${sensitive}" "${report}"; then
    echo "ERROR: evidence report contains token or secret material" >&2
    rm -f "${report}"
    exit 1
  fi
done
if grep -Eq 'v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' "${report}"; then
  echo "ERROR: evidence report contains a Subject token" >&2
  rm -f "${report}"
  exit 1
fi

printf 'Built-package TypeScript SDK Colima acceptance passed\nEvidence: %s\n' "${report}"
