#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

required=(colima kubectl curl openssl go node npm)
missing=()
for command in "${required[@]}"; do
  command -v "${command}" >/dev/null 2>&1 || missing+=("${command}")
done
if ((${#missing[@]})); then
  printf 'ERROR: missing required commands: %s\n' "${missing[*]}" >&2
  exit 1
fi

case "$(uname -s)" in
  Darwin) ;;
  *) echo "ERROR: this Golden Path currently supports macOS only" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "${arch}" in
  arm64|x86_64) ;;
  *) echo "ERROR: unsupported Mac architecture '${arch}'" >&2; exit 1 ;;
esac

if ! colima version >/dev/null 2>&1; then
  echo "ERROR: Colima is installed but not usable" >&2
  exit 1
fi

cat <<EOF
Preflight passed
  profile:       ${COLIMA_PROFILE}
  architecture:  ${arch}
  k3s:           ${KUBERNETES_VERSION}
  gVisor:        ${GVISOR_VERSION}
  Agent Sandbox: ${AGENT_SANDBOX_VERSION}
EOF
