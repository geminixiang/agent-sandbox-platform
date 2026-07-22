#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

image="${CODING_IMAGE:-agent-sandbox-coding:local}"
source_hash="$(cat "${REPO_ROOT}/images/coding/Dockerfile" "${REPO_ROOT}/images/runtime/file-transfer/main.go" | shasum -a 256 | awk '{print $1}')"
image_label="$(colima ssh --profile "${COLIMA_PROFILE}" -- sudo nerdctl --namespace k8s.io image inspect "${image}" 2>/dev/null | sed -n 's/.*"dev.geminixiang.sandbox.source": "\([a-f0-9]*\)".*/\1/p' | head -1 || true)"
if [[ "${image_label}" == "${source_hash}" ]]; then
  echo "Coding image ${image} is already current"
  exit 0
fi
if ! colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "ERROR: run ./scripts/local/up.sh first" >&2
  exit 1
fi

echo "Building ${image} inside Colima profile '${COLIMA_PROFILE}'..."
colima ssh --profile "${COLIMA_PROFILE}" -- sudo nerdctl --namespace k8s.io build \
  --label "dev.geminixiang.sandbox.source=${source_hash}" \
  --tag "${image}" \
  --file "${REPO_ROOT}/images/coding/Dockerfile" \
  "${REPO_ROOT}"
