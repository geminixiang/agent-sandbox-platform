#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK_ROOT="${REPO_ROOT}/packages/sdk-go"
temp_dir="$(mktemp -d)"
trap 'rm -rf "${temp_dir}"' EXIT

cp -R "${SDK_ROOT}" "${temp_dir}/sdk-go"
mkdir "${temp_dir}/consumer"
cat >"${temp_dir}/consumer/go.mod" <<EOF
module example.com/sdk-consumer

go 1.22

require github.com/geminixiang/agent-sandbox-platform/packages/sdk-go v0.2.0-rc.1
replace github.com/geminixiang/agent-sandbox-platform/packages/sdk-go => ../sdk-go
EOF
cat >"${temp_dir}/consumer/sdk_test.go" <<'EOF'
package consumer

import (
  "context"
  "errors"
  "testing"
  sandbox "github.com/geminixiang/agent-sandbox-platform/packages/sdk-go"
)

func TestPublicSurfaceCompiles(t *testing.T) {
  _, _ = sandbox.NewClient(sandbox.ClientOptions{BaseURL:"https://sandbox.example", Credentials:sandbox.StaticToken("short-lived")})
  var command *sandbox.CommandFailedError
  _ = errors.As(sandbox.ErrCommandFailed, &command)
  var _ sandbox.Credentials = sandbox.TokenProviderFunc(func(context.Context)(string,error){ return "token",nil })
  _ = sandbox.Version
}
EOF
(
  cd "${temp_dir}/consumer"
  GOWORK=off go mod tidy
  GOWORK=off go test ./...
  modules="$(GOWORK=off go list -m all)"
  count="$(printf '%s\n' "${modules}" | wc -l | tr -d ' ')"
  [[ "${count}" == "2" ]] || { echo "unexpected module graph:" >&2; printf '%s\n' "${modules}" >&2; exit 1; }
)
if find "${SDK_ROOT}" -maxdepth 1 -name go.sum -print -quit | grep -q .; then
  echo "Go SDK unexpectedly has external dependencies" >&2
  exit 1
fi
printf 'Verified Go SDK external module import: standard library only\n'
