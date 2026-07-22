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
if [[ "${ready:-}" != "1" ]]; then
  echo "ERROR: browser WarmPool did not become ready" >&2
  exit 1
fi

state_dir="${REPO_ROOT}/.sandbox-platform"
credentials="${state_dir}/local.json"
pid_file="${state_dir}/control-plane.pid"
log_file="${state_dir}/control-plane.log"
binary="${state_dir}/control-plane"
mkdir -p "${state_dir}"
chmod 0700 "${state_dir}"

managed_pid=""
if [[ -f "${pid_file}" ]]; then
  managed_pid="$(cat "${pid_file}")"
  if ! kill -0 "${managed_pid}" 2>/dev/null; then
    rm -f "${pid_file}"
    managed_pid=""
  fi
fi

listener_pid="$(lsof -nP -iTCP@127.0.0.1:8787 -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
if [[ -n "${listener_pid}" && "${listener_pid}" != "${managed_pid}" ]]; then
  listener_command="$(ps -p "${listener_pid}" -o command= 2>/dev/null || echo unknown)"
  cat <<EOF >&2
ERROR: 127.0.0.1:8787 is already owned by an unmanaged process.
  PID: ${listener_pid}
  Command: ${listener_command}
Stop it or choose a different local port before running pi-up.sh.
EOF
  exit 1
fi

if [[ -n "${managed_pid}" ]]; then
  if [[ ! -f "${credentials}" ]]; then
    echo "ERROR: managed control plane is running but local credentials are missing; run ./scripts/local/pi-down.sh first" >&2
    exit 1
  fi
  echo "Go control plane is already running (PID ${managed_pid})"
else
  consumer_secret="$(openssl rand -hex 32)"
  metadata_secret="$(openssl rand -hex 32)"
  cat >"${credentials}" <<EOF
{
  "baseUrl": "http://127.0.0.1:8787",
  "consumerId": "pi-local",
  "subjectId": "pi-session",
  "consumerSecret": "${consumer_secret}"
}
EOF
  chmod 0600 "${credentials}"

  echo "Building Go control plane..."
  (cd "${REPO_ROOT}" && go build -o "${binary}" ./apps/control-plane-go/cmd/control-plane)
  echo "Starting Go control plane..."
  env \
    SANDBOX_ADDRESS=127.0.0.1:8787 \
    SANDBOX_K8S_CONTEXT="colima-${COLIMA_PROFILE}" \
    SANDBOX_K8S_NAMESPACE="${PLATFORM_NAMESPACE}" \
    SANDBOX_METADATA_SECRET="${metadata_secret}" \
    SANDBOX_CONSUMER_SECRETS="{\"pi-local\":\"${consumer_secret}\"}" \
    SANDBOX_K8S_POOLS='{"coding":{"warmPoolName":"platform-gvisor","runtimeClassName":"gvisor","containerName":"shell"},"browser":{"warmPoolName":"platform-browser-gvisor","runtimeClassName":"gvisor","containerName":"browser"}}' \
    SANDBOX_SWEEP_INTERVAL=1s \
    "${binary}" >"${log_file}" 2>&1 &
  managed_pid="$!"
  echo "${managed_pid}" >"${pid_file}"
fi

for _ in $(seq 1 200); do
  if ! kill -0 "${managed_pid}" 2>/dev/null; then
    cat "${log_file}" >&2
    rm -f "${pid_file}" "${credentials}"
    echo "ERROR: Go control plane exited" >&2
    exit 1
  fi
  current_listener="$(lsof -nP -iTCP@127.0.0.1:8787 -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
  if [[ "${current_listener}" == "${managed_pid}" ]] && curl -fsS http://127.0.0.1:8787/ready >/dev/null 2>&1; then
    cat <<EOF
Pi Sandbox environment is ready.
  control plane PID: ${managed_pid}

Start or reload pi in this repository:
  pi
  /reload

No SANDBOX_PLATFORM_TOKEN export is required. Local credentials are stored at:
  ${credentials}
EOF
    exit 0
  fi
  sleep .1
done

tail -100 "${log_file}" >&2
kill "${managed_pid}" 2>/dev/null || true
rm -f "${pid_file}" "${credentials}"
echo "ERROR: managed Go control plane did not become ready on 127.0.0.1:8787" >&2
exit 1
