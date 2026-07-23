# Agent Sandbox Go SDK

Standard-library-only Go client for trusted server and worker processes.

```go
client, err := sandbox.NewClient(sandbox.ClientOptions{
    BaseURL: "https://sandbox.example.com",
    Credentials: sandbox.TokenProviderFunc(func(ctx context.Context) (string, error) {
        return tokenBroker.SubjectToken(ctx)
    }),
})
if err != nil { return err }
defer client.Close()

box, err := client.Create(ctx, sandbox.CreateOptions{Pool: "coding"})
if err != nil { return err }
cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
defer box.Close(cleanupCtx)

result, err := box.Run(ctx, "python main.py", sandbox.RunOptions{Check: true})
```

`v0.2.0-rc.1` supports:

- rotating short-lived Subject tokens;
- create, get/connect, paged active discovery;
- foreground command execution with checked diagnostics;
- text and canonical binary convenience methods;
- bounded 64 MiB binary streaming with SHA-256 verification;
- release/delete cleanup and typed errors using `errors.Is`/`errors.As`.

The SDK has no Kubernetes or cloud types and no non-standard-library dependencies. It does not support browsers, Consumer-secret distribution, background command sessions, PTY, workload port exposure, snapshots, or cross-Lease volumes.

Install for the trial from this repository tag/module path after `packages/sdk-go/v0.2.0-rc.1` is published:

```bash
go get github.com/geminixiang/agent-sandbox-platform/packages/sdk-go@v0.2.0-rc.1
```

For an exact retry identity, set `CreateOptions.IdempotencyKey`; otherwise the SDK generates one. `Pager` traverses valid empty intermediate pages. A listed Sandbox can be released concurrently, so `Connect` remains authoritative.

Streaming upload requires exact `SizeBytes` and lowercase SHA-256. A download validates length and digest at normal EOF; early `Close` cancels without integrity verification.
