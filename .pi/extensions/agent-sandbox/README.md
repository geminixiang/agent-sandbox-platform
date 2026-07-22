# Agent Sandbox pi extension

Project-local pi tools for exercising the Agent Sandbox Platform. The extension is auto-discovered when pi starts in this repository.

## Required environment

Start the complete local pi environment with one command:

```bash
./scripts/local/pi-up.sh
pi
```

No manual token export is required. `pi-up.sh` writes development-only credentials to the gitignored, mode-`0600` `.sandbox-platform/local.json`; the extension signs a fresh five-minute Subject token for each request. Stop the local control plane and remove credentials with:

```bash
./scripts/local/pi-down.sh
```

For an external control plane, export:

```bash
export SANDBOX_PLATFORM_URL=https://sandbox.example.com
export SANDBOX_PLATFORM_TOKEN='short-lived signed Subject token'
```

Tools:

- `sandbox_create`
- `sandbox_status`
- `sandbox_exec`
- `sandbox_write_file`
- `sandbox_read_file` returns UTF-8 text directly. For base64 files it returns metadata (`bytes`, `base64Characters`, `sha256`) by default; set `includeContent=true` only for small files.
- `sandbox_browser_run`
- `sandbox_close`

## Secrets

Never pass a secret value to a tool. `sandbox_exec.secretEnv` and `sandbox_browser_run.secretEnv` map a Sandbox variable name to a host environment variable name:

```json
{
  "secretEnv": {
    "GITHUB_TOKEN": "MY_HOST_GITHUB_TOKEN"
  }
}
```

The extension resolves the value locally immediately before the request and redacts it from tool output. The mapping—not the value—is visible to the model. This first version injects secrets per command; it does not persist them into Kubernetes resources or the workspace.

## Browser flow

Create the `browser` Pool, write a Playwright `.mjs` file under `/workspace`, and call `sandbox_browser_run`. The browser runtime image provides Chromium and `playwright-core` under `/opt/browser`.

## Development

```bash
cd .pi/extensions/agent-sandbox
npm install
npm run check
npm test
```

`npm run test:e2e` additionally requires the environment documented in `test/platform.e2e.ts` and a running Go control plane with a `browser` Pool.
