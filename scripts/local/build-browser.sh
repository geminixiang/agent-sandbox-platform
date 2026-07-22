#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=../../deploy/colima/versions.env
source "${REPO_ROOT}/deploy/colima/versions.env"

image="${BROWSER_IMAGE:-agent-sandbox-browser:local}"
if ! colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
  echo "ERROR: run ./scripts/local/up.sh first" >&2
  exit 1
fi

echo "Building ${image} inside Colima profile '${COLIMA_PROFILE}'..."
colima ssh --profile "${COLIMA_PROFILE}" -- sudo nerdctl --namespace k8s.io build \
  --tag "${image}" \
  "${REPO_ROOT}/images/browser"
