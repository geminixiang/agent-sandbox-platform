# Colima development environment

This is the reproducible macOS Golden Path for the production architecture:

```text
TypeScript SDK → Go control plane → Agent Sandbox → gVisor
```

The scripts create an isolated Colima profile named `agent-sandbox-platform`; they do not modify the default profile. The current pins are in [`versions.env`](versions.env).

## Requirements

- macOS on Apple Silicon or Intel
- Colima
- kubectl
- curl and OpenSSL
- Go, Node.js >= 22.19, and npm

Run the preflight check:

```bash
./scripts/local/preflight.sh
```

## Install

```bash
./scripts/local/up.sh
```

The installer is idempotent and:

1. creates a containerd+k3s Colima profile,
2. installs pinned, checksum-verified gVisor binaries inside the VM,
3. configures containerd's `runsc` handler,
4. installs the `gvisor` RuntimeClass,
5. installs the pinned Agent Sandbox controller and CRDs with extensions,
6. applies the local SandboxTemplate and WarmPool.

Run it a second time to verify convergence:

```bash
./scripts/local/up.sh
```

On an already-converged profile, `up.sh` verifies the pinned controller/CRDs instead of downloading and reapplying them. `build-browser.sh` labels the local image with a source hash and skips rebuilding unchanged sources.

## Smoke test

```bash
./scripts/local/smoke.sh
```

The smoke test builds and starts the production Go control plane, drives it through the TypeScript SDK, and verifies:

- Lease acquisition,
- gVisor execution,
- workspace file write/read,
- control-plane restart recovery,
- release and Claim cleanup.

The coding and browser templates run as non-root UID/GID `10001`, disallow privilege escalation, and use the default seccomp profile. The coding image additionally drops all Linux capabilities and uses a read-only root filesystem; `/workspace` is the writable PVC.

## Browser gVisor test

Build the pinned Chromium/Playwright image inside Colima and run a real browser under gVisor:

```bash
./scripts/local/browser-smoke.sh
```

This test claims a browser Sandbox from its WarmPool, verifies the backing Pod uses `RuntimeClass: gvisor`, launches Chromium as a non-root user with its own sandbox enabled, clicks an element through Playwright, and saves a screenshot to the persistent workspace.

## Python wheel browser test

Build the Python SDK wheel, install it into a clean virtual environment, and drive the same real browser path through the Go control plane:

```bash
./scripts/local/python-browser-smoke.sh
```

This verifies the release artifact—not the source checkout—can create a browser Sandbox, write a Playwright module, navigate to a public page, read a screenshot as bytes, and release the Lease.

## Python wheel streaming test

With the existing Colima profile running, build both current runtime images and exercise the binary streaming contract through a clean-installed Python wheel:

```bash
./scripts/local/python-streaming-smoke.sh
```

The script does not create, stop, or delete a Colima profile. It applies the coding and browser Pools, starts a temporary Go control plane with a one-transfer-per-Lease limit, uses dynamically generated five-minute Subject tokens, and covers 32 MiB round trips, atomic replacement, integrity rejection, early-close permit release, release-time aborts, tenant scoping, and symlink rejection. Cleanup is limited to its process, temporary files, and Claims carrying its hashed consumer identity.

Secret-free evidence containing the immutable git commit, Kubernetes image IDs, WarmPool readiness, and before/after Claim sets is verified and written under `.sandbox-platform/test-reports/`. The test intentionally omits a raw-TCP short-`Content-Length` probe: framing behavior varies by HTTP client/server connection handling, while the deterministic mismatch contract is covered by control-plane tests. The installed SDK is used for all other transfers; direct `httpx` is limited to the malformed digest contract case.

## Pi extension environment

Start the complete environment used by the project-local pi extension:

```bash
./scripts/local/pi-up.sh
pi
```

This adds the browser Pool and starts the Go control plane. Local credentials are generated automatically in a gitignored mode-`0600` file; no token export is required. Stop it with:

```bash
./scripts/local/pi-down.sh
```

## Cleanup

Remove Platform and Agent Sandbox cluster resources but retain the Colima VM:

```bash
./scripts/local/down.sh
```

Delete the entire isolated profile and all its data:

```bash
./scripts/local/down.sh --delete-profile
```

Agent Sandbox CRDs are cluster-scoped. `down.sh` removes them only from this dedicated profile; do not point these scripts at a shared cluster.

## Current scope

This phase establishes reproducible local infrastructure. Browser/Chromium images, authenticated CDP routing, and the pi extension are later phases and are intentionally not installed here.
