#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

"${SCRIPT_DIR}/preflight.sh"

context="colima-${COLIMA_PROFILE}"
if colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "Colima profile '${COLIMA_PROFILE}' is already running"
else
  echo "Starting Colima profile '${COLIMA_PROFILE}'..."
  colima start --profile "${COLIMA_PROFILE}" \
    --vm-type "${COLIMA_VM_TYPE}" \
    --runtime "${COLIMA_RUNTIME}" \
    --kubernetes \
    --kubernetes-version "${KUBERNETES_VERSION}" \
    --cpu "${COLIMA_CPU}" \
    --memory "${COLIMA_MEMORY_GIB}" \
    --disk "${COLIMA_DISK_GIB}"
fi

wait_for_cluster() {
  for _ in $(seq 1 120); do
    if kubectl --context "${context}" get node >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  echo "ERROR: Kubernetes context '${context}' did not become ready" >&2
  return 1
}
wait_for_cluster

vm_arch="$(colima ssh --profile "${COLIMA_PROFILE}" -- uname -m | tr -d '\r')"
case "${vm_arch}" in
  aarch64|arm64)
    artifact_arch=aarch64
    runsc_sha256="${GVISOR_RUNSC_SHA256_AARCH64}"
    shim_sha256="${GVISOR_SHIM_SHA256_AARCH64}"
    ;;
  x86_64|amd64)
    artifact_arch=x86_64
    runsc_sha256="${GVISOR_RUNSC_SHA256_X86_64}"
    shim_sha256="${GVISOR_SHIM_SHA256_X86_64}"
    ;;
  *) echo "ERROR: unsupported Colima VM architecture '${vm_arch}'" >&2; exit 1 ;;
esac

install_gvisor() {
  local base_url="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION}/${artifact_arch}"
  local temp_dir
  temp_dir="$(mktemp -d)"
  trap 'rm -rf "${temp_dir}"' RETURN

  echo "Installing gVisor ${GVISOR_VERSION} for ${artifact_arch}..."
  curl -fsSL "${base_url}/runsc" -o "${temp_dir}/runsc"
  curl -fsSL "${base_url}/containerd-shim-runsc-v1" -o "${temp_dir}/containerd-shim-runsc-v1"
  echo "${runsc_sha256}  ${temp_dir}/runsc" | shasum -a 256 -c -
  echo "${shim_sha256}  ${temp_dir}/containerd-shim-runsc-v1" | shasum -a 256 -c -
  chmod +x "${temp_dir}/runsc" "${temp_dir}/containerd-shim-runsc-v1"

  colima ssh --profile "${COLIMA_PROFILE}" -- sudo mkdir -p /usr/local/bin
  cat "${temp_dir}/runsc" | colima ssh --profile "${COLIMA_PROFILE}" -- sudo tee /usr/local/bin/runsc >/dev/null
  cat "${temp_dir}/containerd-shim-runsc-v1" | colima ssh --profile "${COLIMA_PROFILE}" -- sudo tee /usr/local/bin/containerd-shim-runsc-v1 >/dev/null
  colima ssh --profile "${COLIMA_PROFILE}" -- sudo chmod 0755 /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1

  cat <<'EOF' | colima ssh --profile "${COLIMA_PROFILE}" -- sudo tee /etc/containerd/config.toml >/dev/null
version = 3

[grpc]
  gid = 1000

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF
  colima ssh --profile "${COLIMA_PROFILE}" -- sudo systemctl restart containerd
  colima ssh --profile "${COLIMA_PROFILE}" -- sudo systemctl restart k3s
}

current_version="$(colima ssh --profile "${COLIMA_PROFILE}" -- sh -lc 'runsc --version 2>/dev/null | head -1' || true)"
if [[ "${current_version}" != *"release-${GVISOR_VERSION}"* ]]; then
  install_gvisor
  wait_for_cluster
else
  echo "gVisor ${GVISOR_VERSION} is already installed"
fi

kubectl --context "${context}" apply -f "${REPO_ROOT}/deploy/colima/runtimeclass-gvisor.yaml"

manifest="$(mktemp)"
trap 'rm -f "${manifest}"' EXIT
manifest_url="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml"
echo "Installing Agent Sandbox ${AGENT_SANDBOX_VERSION}..."
curl -fsSL "${manifest_url}" -o "${manifest}"
echo "${AGENT_SANDBOX_MANIFEST_SHA256}  ${manifest}" | shasum -a 256 -c -
kubectl --context "${context}" apply --server-side --force-conflicts -f "${manifest}"
kubectl --context "${context}" -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s

kubectl --context "${context}" apply -f "${REPO_ROOT}/deploy/colima/e2e.yaml"
echo "Waiting for the coding WarmPool..."
for _ in $(seq 1 120); do
  ready_replicas="$(kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandboxwarmpool platform-gvisor -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready_replicas}" == "1" ]] && break
  sleep 1
done
if [[ "${ready_replicas:-}" != "1" ]]; then
  kubectl --context "${context}" -n "${PLATFORM_NAMESPACE}" get sandboxwarmpool,sandbox,pod >&2
  echo "ERROR: coding WarmPool did not become ready" >&2
  exit 1
fi

cat <<EOF
Local platform prerequisites are ready.
  context:   ${context}
  namespace: ${PLATFORM_NAMESPACE}

Run:
  ./scripts/local/smoke.sh
EOF
