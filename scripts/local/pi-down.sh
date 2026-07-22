#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
state_dir="${REPO_ROOT}/.sandbox-platform"
pid_file="${state_dir}/control-plane.pid"

if [[ -f "${pid_file}" ]]; then
  pid="$(cat "${pid_file}")"
  if kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}"
    for _ in $(seq 1 100); do kill -0 "${pid}" 2>/dev/null || break; sleep .1; done
  fi
fi
rm -rf "${state_dir}"
echo "Pi Sandbox control plane stopped and local credentials removed."
