# Mikan trial quickstart

## Trial goal

Connect one Mikan server/worker to the Agent Sandbox Platform without importing Kubernetes concepts. Mikan supplies a short-lived Subject token and a logical Pool; the platform owns Pod, RuntimeClass, node, Workspace, and cleanup details.

Supported in this trial:

- `coding` and `browser` Pools;
- foreground commands with typed failures;
- text, bytes, and bounded 64 MiB streaming files;
- active Sandbox discovery and reconnect;
- release/delete lifecycle;
- TypeScript `0.2.0-rc.1` and Go `v0.2.0-rc.1`.

Not supported: background command handles, PTY, workload URLs/ports, snapshots/fork, cross-Lease volumes, or HA control-plane replicas.

## Required configuration

Mikan's trusted backend receives only:

```text
SANDBOX_PLATFORM_URL=https://sandbox.example.internal
SANDBOX_SUBJECT_TOKEN=<short-lived token>
```

Do not put the Consumer signing secret in Mikan browser code, Agent prompts, model context, logs, or repository configuration. The production integration should obtain five-minute Subject tokens from an internal token broker.

## TypeScript

Install the RC when published:

```bash
npm install @geminixiang/sandbox-sdk@0.2.0-rc.1
```

```ts
import {
  CommandFailedError,
  SandboxClient,
  StaticToken,
} from "@geminixiang/sandbox-sdk";

await using client = new SandboxClient({
  baseUrl: process.env.SANDBOX_PLATFORM_URL!,
  credentials: new StaticToken(process.env.SANDBOX_SUBJECT_TOKEN!),
});

const output = await client.sandbox(
  { pool: "coding", ttlSeconds: 300 },
  async (sandbox) => {
    await sandbox.files.writeText(
      "/workspace/main.py",
      "print('hello from Mikan')",
    );
    const result = await sandbox.run("python main.py", {
      cwd: "/workspace",
      check: true,
    });
    return result.stdout;
  },
);

console.log(output);
```

For a real token broker, replace `StaticToken` with an async provider. It is called for every operation:

```ts
credentials: async ({ signal }) => tokenBroker.subjectToken({ signal })
```

Catch diagnostics without losing output:

```ts
try {
  await sandbox.run("python broken.py", { check: true });
} catch (error) {
  if (error instanceof CommandFailedError) {
    console.error(error.result.exitCode, error.result.stderr);
  }
  throw error;
}
```

## Go

Install the RC after the repository module tag is published:

```bash
go get github.com/geminixiang/agent-sandbox-platform/packages/sdk-go@v0.2.0-rc.1
```

```go
client, err := sandbox.NewClient(sandbox.ClientOptions{
    BaseURL: os.Getenv("SANDBOX_PLATFORM_URL"),
    Credentials: sandbox.StaticToken(os.Getenv("SANDBOX_SUBJECT_TOKEN")),
})
if err != nil { return err }
defer client.Close()

box, err := client.Create(ctx, sandbox.CreateOptions{
    Pool: "coding",
    TTLSeconds: 300,
})
if err != nil { return err }

cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
defer box.Close(cleanupCtx)

if err := box.Files().WriteText(ctx, "/workspace/main.py", "print('hello from Mikan')"); err != nil {
    return err
}
result, err := box.Run(ctx, "python main.py", sandbox.RunOptions{
    CWD: "/workspace",
    Check: true,
})
if err != nil { return err }
fmt.Print(result.Stdout)
```

Use `errors.As` for command diagnostics and `errors.Is` for stable categories:

```go
var commandErr *sandbox.CommandFailedError
if errors.As(err, &commandErr) {
    log.Print(commandErr.Result.Stderr)
}
if errors.Is(err, sandbox.ErrNotFound) {
    // The reconnect target was released or expired.
}
```

## Thin adapter shape

Mikan should keep the platform behind one internal adapter:

```ts
type SandboxTask = {
  pool: "coding" | "browser";
  command: string;
  cwd?: string;
  timeoutSeconds?: number;
  files?: Array<{ path: string; content: string }>;
};

type SandboxTaskResult = {
  sandboxId: string;
  stdout: string;
  stderr: string;
  exitCode: number;
};
```

The adapter performs create → write inputs → run → collect result → release. It should forward the Mikan request cancellation signal and use an explicit idempotency key derived from the Mikan job/tool-call ID.

## Trial checklist

1. Run one `coding` task that writes and executes a file.
2. Run one `browser` task using `/opt/browser/smoke.mjs` or a Mikan Playwright script.
3. Cancel one request and confirm Mikan receives the typed abort category.
4. Reconnect to an active Sandbox by ID, then release it.
5. Confirm no credentials or file contents appear in Mikan logs.

## Feedback to capture

Record these five facts for each trial workflow:

1. Pool and task type.
2. Acquire and command duration.
3. Largest input/output file.
4. Error code and whether diagnostics were sufficient.
5. Whether the missing feature was background commands, PTY, workload URL, persistent volume, or something else.

Do not treat local benchmark numbers as an SLO. Report platform issues with request/Lease IDs and timestamps, never tokens or file contents.
