#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

context="colima-${COLIMA_PROFILE}"
namespace="agent-sandbox-command-supervisor-gate"
pool="command-supervisor-gvisor"
image="agent-sandbox-command-supervisor:stage-6.0-gate"
manifest="${REPO_ROOT}/deploy/colima/command-supervisor-gate.yaml"
timestamp="$(date -u +%Y-%m-%dT%H%M%SZ)"
report_root="${REPO_ROOT}/.sandbox-platform/test-reports"
artifact_dir="${report_root}/command-supervisor-${timestamp}"
report="${report_root}/command-supervisor-${timestamp}.json"
temp_dir="$(mktemp -d)"
claim=""
pod=""
cleanup_done=0

mkdir -p "${artifact_dir}"

snapshot_existing_pools() {
  kubectl --context "${context}" get sandboxwarmpools -A -o json \
    | jq --arg namespace "${namespace}" '[.items[] | select(.metadata.namespace != $namespace) | {namespace:.metadata.namespace,name:.metadata.name,resourceVersion:.metadata.resourceVersion,spec:.spec}] | sort_by(.namespace,.name)'
}

capture_diagnostics() {
  if kubectl --context "${context}" get namespace "${namespace}" >/dev/null 2>&1; then
    kubectl --context "${context}" -n "${namespace}" get sandboxwarmpools,sandboxtemplates,sandboxclaims,sandboxes,pods,pvc -o wide >"${artifact_dir}/resources.txt" 2>&1 || true
    kubectl --context "${context}" -n "${namespace}" get events -o json >"${artifact_dir}/events.json" 2>/dev/null || true
    local diagnostic_pod="${pod}"
    if [[ -z "${diagnostic_pod}" ]]; then
      diagnostic_pod="$(kubectl --context "${context}" -n "${namespace}" get pods -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    fi
    if [[ -n "${diagnostic_pod}" ]]; then
      kubectl --context "${context}" -n "${namespace}" logs "${diagnostic_pod}" -c supervisor >"${artifact_dir}/supervisor.log" 2>&1 || true
      kubectl --context "${context}" -n "${namespace}" logs "${diagnostic_pod}" -c supervisor --previous >"${artifact_dir}/supervisor-previous.log" 2>&1 || true
    fi
  fi
}

cleanup() {
  if [[ "${cleanup_done}" == "1" ]]; then
    return
  fi
  if [[ -n "${claim}" ]]; then
    kubectl --context "${context}" -n "${namespace}" delete sandboxclaim "${claim}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  fi
  kubectl --context "${context}" delete namespace "${namespace}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  if ! kubectl --context "${context}" get namespace "${namespace}" >/dev/null 2>&1; then
    cleanup_done=1
  fi
}

on_exit() {
  local rc=$?
  if [[ "${cleanup_done}" != "1" ]]; then
    capture_diagnostics
    cleanup
  fi
  rm -rf "${temp_dir}"
  if [[ ${rc} -ne 0 ]]; then
    printf 'Command supervisor gate failed unexpectedly. Diagnostics: %s\n' "${artifact_dir}" >&2
  fi
}
trap on_exit EXIT

if ! colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "ERROR: Colima profile '${COLIMA_PROFILE}' is not running; run ./scripts/local/up.sh" >&2
  exit 1
fi
kubectl --context "${context}" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=60s

kubectl --context "${context}" delete namespace "${namespace}" --ignore-not-found --wait=true >/dev/null
snapshot_existing_pools >"${temp_dir}/warm-pools-before.json"

commit="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
tree_state="clean"
[[ -n "$(git -C "${REPO_ROOT}" status --porcelain=v1)" ]] && tree_state="dirty"

printf 'Building isolated supervisor gate image %s...\n' "${image}"
colima ssh --profile "${COLIMA_PROFILE}" -- sudo nerdctl --namespace k8s.io build \
  --label "dev.geminixiang.sandbox.commit=${commit}" \
  --tag "${image}" \
  --file "${REPO_ROOT}/images/command-supervisor/Dockerfile" \
  "${REPO_ROOT}" | tee "${artifact_dir}/image-build.log"
expected_image_id="$(colima ssh --profile "${COLIMA_PROFILE}" -- sudo k3s crictl inspecti "${image}" | jq -r '.status.id')"
[[ -n "${expected_image_id}" && "${expected_image_id}" != "null" ]] || { echo "ERROR: built image has no immutable ID" >&2; exit 1; }

kubectl --context "${context}" apply -f "${manifest}"
for _ in $(seq 1 180); do
  ready="$(kubectl --context "${context}" -n "${namespace}" get sandboxwarmpool "${pool}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready}" == "1" ]] && break
  sleep 1
done
if [[ "${ready:-}" != "1" ]]; then
  echo "ERROR: isolated command supervisor WarmPool did not become ready" >&2
  exit 1
fi

claim="command-supervisor-gate-$(openssl rand -hex 4)"
kubectl --context "${context}" -n "${namespace}" apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: ${claim}
spec:
  warmPoolRef:
    name: ${pool}
EOF
for _ in $(seq 1 120); do
  sandbox="$(kubectl --context "${context}" -n "${namespace}" get sandboxclaim "${claim}" -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"
  [[ -n "${sandbox}" ]] && break
  sleep 1
done
[[ -n "${sandbox:-}" ]] || { echo "ERROR: isolated SandboxClaim was not satisfied" >&2; exit 1; }
pod="$(kubectl --context "${context}" -n "${namespace}" get sandbox "${sandbox}" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}' 2>/dev/null || true)"
pod="${pod:-${sandbox}}"
kubectl --context "${context}" -n "${namespace}" wait --for=condition=Ready "pod/${pod}" --timeout=180s

runtime_class="$(kubectl --context "${context}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.spec.runtimeClassName}')"
actual_image_id="$(kubectl --context "${context}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="supervisor")].imageID}')"
[[ "${runtime_class}" == "gvisor" ]] || { echo "ERROR: expected gvisor, got '${runtime_class}'" >&2; exit 1; }
[[ "${actual_image_id}" == "${expected_image_id}" ]] || { echo "ERROR: Pod imageID '${actual_image_id}' does not equal built imageID '${expected_image_id}'" >&2; exit 1; }

set +e
kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c supervisor -- /usr/local/bin/agent-sandbox-platform-gate >"${temp_dir}/core.json"
core_rc=$?
set -e
cp "${temp_dir}/core.json" "${artifact_dir}/core.json"
if [[ ${core_rc} -ne 0 ]] || ! jq -e '.status == "blocked" or .status == "passed"' "${temp_dir}/core.json" >/dev/null; then
  echo "ERROR: in-Sandbox core gate had an unexpected failure" >&2
  exit 1
fi

ctl() {
  kubectl --context "${context}" -n "${namespace}" exec -i "${pod}" -c supervisor -- /usr/local/bin/agent-sandbox-ctl
}

restart_marker="/workspace/supervisor-restart-marker"
restart_request="$(jq -nc --arg marker "${restart_marker}" '{version:1,operation:"start",requestId:"gate-supervisor-restart",argv:["/bin/sh","-c",("while :; do printf x >> " + $marker + "; sleep .05; done")],cwd:"/workspace"}')"
restart_response="$(printf '%s' "${restart_request}" | ctl)"
restart_id="$(jq -er 'select(.ok == true) | .command.id' <<<"${restart_response}")"
for _ in $(seq 1 100); do
  marker_before="$(kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c supervisor -- sh -c "wc -c < '${restart_marker}'" 2>/dev/null || true)"
  [[ "${marker_before:-0}" -gt 2 ]] && break
  sleep .05
done
[[ "${marker_before:-0}" -gt 2 ]] || { echo "ERROR: restart marker command did not run" >&2; exit 1; }
restart_count_before="$(kubectl --context "${context}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="supervisor")].restartCount}')"

# Crash only the supervisor child. tini exits with it, so Kubernetes restarts the
# same container while the Pod's emptyDir and workspace PVC remain mounted.
kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c supervisor -- sh -c '
  for comm in /proc/[0-9]*/comm; do
    if [ "$(cat "$comm" 2>/dev/null)" = "agent-sandbox-s" ]; then
      pid="${comm#/proc/}"; pid="${pid%/comm}"; kill -KILL "$pid"; exit 0
    fi
  done
  exit 1
' >/dev/null 2>&1 || true

for _ in $(seq 1 180); do
  restart_count_after="$(kubectl --context "${context}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="supervisor")].restartCount}' 2>/dev/null || true)"
  ready_condition="$(kubectl --context "${context}" -n "${namespace}" get pod "${pod}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  if [[ "${restart_count_after:-0}" -gt "${restart_count_before}" && "${ready_condition}" == "True" ]]; then
    break
  fi
  sleep 1
done
if [[ "${restart_count_after:-0}" -le "${restart_count_before}" || "${ready_condition:-}" != "True" ]]; then
  echo "ERROR: supervisor container did not restart and recover" >&2
  exit 1
fi

lost_response="$(jq -nc --arg id "${restart_id}" '{version:1,operation:"status",commandId:$id}' | ctl)"
[[ "$(jq -r '.command.state // ""' <<<"${lost_response}")" == "lost" ]] || { echo "ERROR: recovered command was not marked lost" >&2; exit 1; }
marker_after_restart="$(kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c supervisor -- sh -c "wc -c < '${restart_marker}'")"
sleep 1
marker_after_wait="$(kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c supervisor -- sh -c "wc -c < '${restart_marker}'")"
[[ "${marker_after_restart}" == "${marker_after_wait}" ]] || { echo "ERROR: old command survived supervisor/container restart" >&2; exit 1; }

successor_response="$(jq -nc '{version:1,operation:"start",requestId:"gate-restart-successor",argv:["/bin/sleep","60"],cwd:"/workspace"}' | ctl)"
successor_id="$(jq -er '.command.id' <<<"${successor_response}")"
# Re-reading old state must not signal any newly owned process, and persisted
# metadata must contain no PID/PGID identity that could be reused.
printf '%s' "$(jq -nc --arg id "${restart_id}" '{version:1,operation:"status",commandId:$id}')" | ctl >/dev/null
successor_state="$(jq -nc --arg id "${successor_id}" '{version:1,operation:"status",commandId:$id}' | ctl | jq -r '.command.state')"
[[ "${successor_state}" == "running" ]] || { echo "ERROR: old recovery path disturbed a new command" >&2; exit 1; }
if kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c supervisor -- sh -c "grep -R -E '\"(pid|pgid)\"' /run/agent-sandbox-supervisor/commands" >/dev/null 2>&1; then
  echo "ERROR: persisted command metadata exposed PID/PGID" >&2
  exit 1
fi
jq -nc --arg id "${successor_id}" '{version:1,operation:"signal",commandId:$id,signal:"KILL"}' | ctl >/dev/null
jq -nc --arg id "${successor_id}" '{version:1,operation:"wait",commandId:$id,timeoutMs:5000}' | ctl >/dev/null
jq -n --argjson before "${restart_count_before}" --argjson after "${restart_count_after}" '{status:"passed",containerRestartCountBefore:$before,containerRestartCountAfter:$after,recoveredState:"lost",oldMarkerStopped:true,persistedProcessIdentity:false,newCommandUnaffected:true}' >"${temp_dir}/restart.json"

capture_diagnostics
cleanup

snapshot_existing_pools >"${temp_dir}/warm-pools-after.json"
if ! cmp -s "${temp_dir}/warm-pools-before.json" "${temp_dir}/warm-pools-after.json"; then
  echo "ERROR: an existing WarmPool resourceVersion or spec changed during the isolated gate" >&2
  diff -u "${temp_dir}/warm-pools-before.json" "${temp_dir}/warm-pools-after.json" >"${artifact_dir}/warm-pool-diff.txt" || true
  exit 1
fi
namespace_exists=false
kubectl --context "${context}" get namespace "${namespace}" >/dev/null 2>&1 && namespace_exists=true
residue_count="$(kubectl --context "${context}" get sandboxclaims,sandboxes,pods,pvc -A -o json | jq --arg namespace "${namespace}" '[.items[] | select(.metadata.namespace == $namespace)] | length')"
[[ "${namespace_exists}" == "false" && "${residue_count}" == "0" ]] || { echo "ERROR: prototype cleanup left namespace resources" >&2; exit 1; }
jq -n --argjson namespaceExists "${namespace_exists}" --argjson residueCount "${residue_count}" '{status:"passed",namespaceExists:$namespaceExists,claimPvcPodSandboxResidue:$residueCount,existingWarmPoolResourceVersionsAndSpecsUnchanged:true}' >"${temp_dir}/cleanup.json"

colima_version="$(colima version | tr '\n' ' ' | tr -s ' ')"
kubernetes_version="$(kubectl --context "${context}" version -o json | jq -c '{clientVersion,serverVersion}')"
node_name="$(kubectl --context "${context}" get nodes -o jsonpath='{.items[0].metadata.name}')"
container_runtime_version="$(kubectl --context "${context}" get node "${node_name}" -o jsonpath='{.status.nodeInfo.containerRuntimeVersion}')"
gvisor_actual_version="$(colima ssh --profile "${COLIMA_PROFILE}" -- sh -lc 'runsc --version 2>/dev/null | head -1' | tr -d '\r')"
[[ "${gvisor_actual_version}" == *"release-${GVISOR_VERSION}"* ]] || { echo "ERROR: actual gVisor version does not match the pinned version" >&2; exit 1; }
agent_sandbox_controller_image="$(kubectl --context "${context}" -n agent-sandbox-system get deployment agent-sandbox-controller -o jsonpath='{.spec.template.spec.containers[0].image}')"
jq -n \
  --arg commit "${commit}" \
  --arg treeState "${tree_state}" \
  --arg context "${context}" \
  --arg namespace "${namespace}" \
  --arg profile "${COLIMA_PROFILE}" \
  --arg colimaVersion "${colima_version}" \
  --argjson kubernetesVersion "${kubernetes_version}" \
  --arg gvisorVersionPin "${GVISOR_VERSION}" \
  --arg gvisorVersionActual "${gvisor_actual_version}" \
  --arg agentSandboxVersion "${AGENT_SANDBOX_VERSION}" \
  --arg agentSandboxControllerImage "${agent_sandbox_controller_image}" \
  --arg imageID "${actual_image_id}" \
  --arg runtimeClass "${runtime_class}" \
  --arg nodeName "${node_name}" \
  --arg containerRuntimeVersion "${container_runtime_version}" \
  --arg artifacts "${artifact_dir#${REPO_ROOT}/}" \
  --slurpfile core "${temp_dir}/core.json" \
  --slurpfile restart "${temp_dir}/restart.json" \
  --slurpfile cleanupProof "${temp_dir}/cleanup.json" \
  '{schemaVersion:1,test:"command-supervisor-colima-gate",status:(if $core[0].status == "blocked" then "blocked" else "passed" end),git:{commit:$commit,treeState:$treeState},environment:{context:$context,prototypeNamespace:$namespace,colimaProfile:$profile,colimaVersion:$colimaVersion,kubernetes:$kubernetesVersion,gvisorVersionPin:$gvisorVersionPin,gvisorVersionActual:$gvisorVersionActual,agentSandboxVersion:$agentSandboxVersion,agentSandboxControllerImage:$agentSandboxControllerImage,runtimeClass:$runtimeClass,nodeName:$nodeName,containerRuntimeVersion:$containerRuntimeVersion,imageID:$imageID},core:$core[0],restart:$restart[0],cleanup:$cleanupProof[0],artifacts:$artifacts}' >"${report}"

jq -e '
  (.status == "blocked" or .status == "passed") and
  (.git.commit | test("^[0-9a-f]{40}$")) and
  (.environment.imageID | startswith("sha256:")) and
  .restart.status == "passed" and
  .cleanup.status == "passed" and
  .cleanup.namespaceExists == false and
  .cleanup.claimPvcPodSandboxResidue == 0 and
  .cleanup.existingWarmPoolResourceVersionsAndSpecsUnchanged == true
' "${report}" >/dev/null

printf 'Command supervisor Stage 6.0 gate status: %s\nEvidence: %s\nArtifacts: %s\n' "$(jq -r '.status' "${report}")" "${report}" "${artifact_dir}"
if [[ "$(jq -r '.status' "${report}")" == "blocked" ]]; then
  echo "Production remains BLOCKED: no enabled mechanism contained the new-session descendant."
fi
