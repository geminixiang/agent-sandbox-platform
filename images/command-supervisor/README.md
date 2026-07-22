# Trusted command supervisor prototype

This image is the isolated Stage 6.0 gate from [ADR 0005](../../docs/adr/0005-durable-command-runtime-prototype.md). It is not wired into `/v1`, either SDK, the production backend seam, Helm defaults, or existing Colima Pools.

`agent-sandbox-supervisor` is the single process owner. `agent-sandbox-ctl` forwards one JSON request from stdin over the root-only Unix socket and prints one JSON response. The supervisor must run on Linux as root in a container that supplies only `SETUID`, `SETGID`, and `KILL`; the program does not add capabilities. Children are re-executed through a small Linux trampoline that sets `no_new_privs`, clears ambient capabilities, drops supplementary groups, changes to UID/GID 10001, and enters a new process group.

State defaults to `/run/agent-sandbox-supervisor`, separate from untrusted `/workspace`. The manager acquires a non-blocking singleton `flock` before loading, recovering, truncating, or mutating command state. Directories use mode `0700`, the socket and files use mode `0600`, and Linux `SO_PEERCRED` admits only UID 0. Records never persist or return PID/PGID values. Persisted `starting` and `running` records become `lost` without signalling any process identity.

## Production containment blocker

The prototype signals a command's Linux process group. This kills ordinary descendants that remain in that PGID, but it **does not contain a descendant that calls `setsid(2)`** and enters a new session/process group. Process-tree polling is intentionally not used because it is racy and would not provide a production guarantee. Runtime core therefore uses a small per-command containment interface—configure spawn, capture the started command, signal the containment target—so a cgroup v2 adapter can replace PGID signalling without changing lifecycle code. Such an adapter appears feasible through a delegated command cgroup plus `cgroup.kill`, but creating/delegating that cgroup is privileged host/container setup and is deliberately not attempted by runtime-core tests. It requires a later environment feasibility gate. Until that gate passes, this unresolved `setsid` escape blocks production use and every claim of complete descendant cleanup.

## Bounded persistence

Each command reserves a maximum of 8 MiB of application-managed state:

- 64 KiB is held back for the atomic metadata old/new files, directory entries, and segment bookkeeping;
- metadata JSON itself is rejected above 8 KiB;
- the remaining bytes contain version-tagged 256 KiB spool segments, including the segment version marker and each event's 13-byte sequence/stream/length header;
- rotation evicts whole old segments before an append can cross the budget;
- a partial final record after a crash is truncated during recovery.

The 64 KiB reserve makes metadata and segment overhead part of the configured cap rather than allowing payload alone to consume 8 MiB. Filesystem block accounting can be more conservative than logical file bytes and remains an environment property measured by the gate.

## Local protocol example

```sh
printf '%s' '{"version":1,"operation":"health"}' \
  | agent-sandbox-ctl

printf '%s' '{"version":1,"operation":"start","requestId":"example-1","argv":["sh","-c","printf hello"],"cwd":"/workspace"}' \
  | agent-sandbox-ctl
```

The request envelope is capped at 1 MiB, stdin writes at 64 KiB per request, output events at 16 KiB, spooled state at 8 MiB per command, and command records at 256. `connect.after` is exclusive; an evicted cursor returns `CURSOR_EXPIRED` and a future cursor returns `INVALID_CURSOR`.

Build the prototype only:

```sh
docker build -f images/command-supervisor/Dockerfile -t agent-sandbox-command-supervisor:stage-6.0 .
```

## Colima feasibility gate

With the pinned Colima profile already running, execute the isolated gate from the repository root:

```sh
./scripts/local/command-supervisor-gate.sh
```

The image includes `agent-sandbox-platform-gate` solely for this test. It drives every lifecycle operation through a fresh `agent-sandbox-ctl` process, directly probes child credentials/capabilities and cgroup exposure, observes how process-group signalling handles a `setsid` descendant, and emits structured evidence. The host script separately verifies supervisor restart recovery, immutable image identity, resource cleanup, and unchanged existing WarmPools. Reports and diagnostics are placed under gitignored `.sandbox-platform/test-reports/`.

This remains a non-production prototype. A `blocked` report exits zero when the core checks passed but no enabled mechanism contained the observed new-session descendant. The gate inspects only the container's current cgroup v2 directory and records writable child-subtree and `cgroup.kill` exposure; this build deliberately enables no cgroup adapter. Any unexpected core, restart, image, cleanup, or invariance failure exits nonzero. Do not use this image as secure production isolation or publish its protocol through `/v1` or an SDK. No outcome is claimed until the real gate is run.
