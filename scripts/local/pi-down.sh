#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
state_dir="${REPO_ROOT}/.sandbox-platform"
pid_file="${state_dir}/control-plane.pid"

if [[ -f "${pid_file}" ]]; then
  pid="$(cat "${pid_file}")"
  listener_pid="$(lsof -nP -iTCP@127.0.0.1:8787 -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
  if kill -0 "${pid}" 2>/dev/null && [[ "${listener_pid}" == "${pid}" ]]; then
    kill "${pid}"
    for _ in $(seq 1 100); do kill -0 "${pid}" 2>/dev/null || break; sleep .1; done
  elif [[ -n "${listener_pid}" ]]; then
    echo "WARNING: refusing to stop unmanaged listener PID ${listener_pid} on 127.0.0.1:8787" >&2
  fi
fi
rm -rf "${state_dir}"
echo "Pi Sandbox control plane stopped and local credentials removed."
