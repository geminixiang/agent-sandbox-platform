# ADR 0005: Gate durable commands on a trusted Sandbox supervisor

- Status: Accepted for prototype; production decision pending gate evidence
- Date: 2026-07-23

## Context

The current execution contract runs one foreground command through Kubernetes Pod exec and buffers its result. It cannot truthfully provide background handles, stdin reconnection, output replay, or command discovery after an SDK or control-plane restart.

A durable command interface needs a process owner inside each Sandbox. A control-plane goroutine, long-lived Pod exec session, `nohup`, PID file, or FIFO cannot provide that ownership:

- transport disconnect does not establish whether a child still runs;
- PID reuse makes persisted PID identity unsafe;
- stdin and output pipes cannot be reconstructed from PID files;
- control-plane restart loses process state;
- a Pod replacement destroys all process memory.

The platform's coding and browser workloads currently run entirely as UID/GID 10001 with all Linux capabilities dropped. If a supervisor runs as the same identity, customer commands can signal it, open its Unix socket, and modify its registry or output spool. Command limits, status, replay, and kill would then be tenant-controlled advisory behavior rather than platform-enforced guarantees.

## Decision

Before publishing a command-session HTTP or SDK interface, build and run an isolated Stage 6.0 prototype gate for a trusted, per-Sandbox command supervisor.

The prototype is not a production feature. It must not change:

- `/v1` HTTP contracts;
- Python or TypeScript public SDKs;
- the Go production backend seam;
- existing Helm or Colima Pool defaults;
- current `/exec` behavior.

### Trust model

The supervisor runs as UID/GID 0 with:

- `allowPrivilegeEscalation: false`;
- read-only root filesystem;
- all capabilities dropped except `SETUID`, `SETGID`, and `KILL`;
- `seccompProfile: RuntimeDefault`;
- no Kubernetes ServiceAccount token.

`SETUID` and `SETGID` are required to spawn customer children as UID/GID 10001. `KILL` is required because Linux does not permit a different-UID parent to signal a child process group merely because it created that child.

Customer children run with:

- UID/GID 10001;
- no supplementary privileged groups;
- no effective, permitted, inheritable, or ambient capabilities;
- `no_new_privs`;
- a separate process group.

Supervisor state and its Unix socket are root-owned and mode `0700`/`0600`. The supervisor verifies `SO_PEERCRED` and accepts only UID 0 clients. The control-plane-like prototype caller reaches a root `ctl` binary through Pod exec. Customer commands must be unable to traverse state directories, open the socket, signal the supervisor, or regain UID 0.

This capability set is a prototype hypothesis, not yet an approved production security policy. If gVisor or cluster admission cannot support it without broader privilege, Stage 6 stops.

### Runtime durability semantics

A single supervisor owns all command children, stdin pipes, process groups, state transitions, and output spools.

Durability means commands and retained output survive:

- SDK disconnect;
- individual `ctl` process exit;
- HTTP observer disconnect in a future control plane;
- Go control-plane restart.

Durability does not mean process execution survives:

- supervisor crash;
- container restart;
- Pod replacement;
- loss of the runtime state volume.

On supervisor startup, persisted `starting` or `running` commands become the terminal outcome `lost`. The implementation must never signal a persisted PID after restart.

### Prototype protocol

Each `ctl` invocation reads one bounded JSON request from stdin and writes one JSON response to stdout. It forwards the request over a length-prefixed Unix socket protocol.

Required operations are:

- `health`;
- `start` with an idempotency request ID;
- `list` and `status`;
- `connect` after an event cursor;
- `stdin` and `closeStdin`;
- `signal` with `TERM` or `KILL`;
- `wait`;
- `killAll`.

Command IDs are cryptographically random and are never PIDs. A repeated start request with the same normalized specification returns the same command ID; a different specification returns `IDEMPOTENCY_CONFLICT`.

Commands use explicit argv. The prototype does not implicitly invoke a shell.

### Events and replay

Stdout and stderr events share one monotonically increasing per-command sequence. Their ordering is the supervisor's observation order, not a claim about kernel-wide temporal ordering.

A terminal state is recorded only after output pipes reach EOF. Output is chunked and stored in a bounded, versioned spool. A cursor older than retained output returns `CURSOR_EXPIRED`; a cursor ahead of generated output returns `INVALID_CURSOR`. Slow observers do not create unbounded supervisor memory.

Prototype defaults are deliberately bounded:

- 16 KiB output events;
- 8 MiB total spool;
- 256 command records;
- 1 MiB request envelope.

These are prototype limits, not final Pool policy.

## Prototype gate

A dedicated Colima namespace, image, SandboxTemplate, and WarmPool must verify under the pinned gVisor runtime:

1. root `ctl` is accepted and UID 10001 socket access is denied;
2. `SETUID`/`SETGID`-only cannot signal the child group, while adding only `KILL` succeeds;
3. children have UID/GID 10001, zero capabilities, and `no_new_privs`;
4. stable command IDs and idempotent start prevent duplicate execution;
5. stdout/stderr sequences are unique and replayable across new `ctl` processes;
6. spool eviction stays bounded and expires old cursors;
7. stdin, close, wait, TERM, KILL, descendants, and kill-all behave deterministically;
8. supervisor restart marks running commands `lost` and the old process no longer runs;
9. no command survives prototype Claim/namespace cleanup;
10. existing production-like Colima WarmPools are not changed.

The report must record environment versions, immutable image ID, individual gate outcomes, failures and fixes, and cleanup proof without credentials or customer output.

Any failed gate blocks Stage 6.1. A failed prototype is valid engineering evidence and must not be re-described as partial production support.

## Consequences

### Positive

- Public command semantics will be based on a real process owner rather than transport accidents.
- Tenant commands cannot silently rewrite platform command history or limits if the gate passes.
- Supervisor and Pod replacement have an explicit `lost` outcome.
- A future control plane can remain Kubernetes-neutral at its public seam while using Pod exec only as an internal adapter.

### Costs and risks

- Production command Pools would need a narrowly privileged root supervisor, unlike current all-non-root Pools.
- Long-lived event observers consume Kubernetes exec connections until a different internal transport is justified.
- Durable replay requires bounded disk state and crash-safe writes.
- A dedicated runtime-state volume may be needed before production; `/workspace` is not trusted supervisor state.
- Kubernetes Pod replacement cannot preserve running processes.

## Deferred

Until the gate passes, do not implement or claim:

- public background command handles;
- SSE command events;
- SDK stdin/reconnect APIs;
- PTY or terminal resize;
- commands surviving Pod replacement;
- unlimited output retention;
- migration of legacy `/exec` onto the supervisor.
