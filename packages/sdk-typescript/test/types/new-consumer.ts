import {
  CommandFailedError,
  SandboxAbortedError,
  SandboxClient,
  StaticToken,
  type CommandResult,
  type TokenProvider,
} from "@geminixiang/sandbox-sdk";

const rotating: TokenProvider = async (context) => {
  context?.signal.throwIfAborted();
  return "short-lived-subject-token";
};

async function consume(): Promise<void> {
  await using client = new SandboxClient({
    baseUrl: new URL("https://sandbox.example"),
    credentials: rotating,
    timeoutMs: 10_000,
  });
  const page = await client.listPage({ pool: "coding", signal: AbortSignal.timeout(1_000) });
  page.sandboxes satisfies readonly import("@geminixiang/sandbox-sdk").Sandbox[];

  await client.sandbox({ pool: "coding", ttlSeconds: 60 }, async (sandbox) => {
    const result: CommandResult = await sandbox.run("printf ok", { check: true, timeoutMs: 1_000 });
    result.exitCode satisfies number;
    result.succeeded satisfies boolean;
    await sandbox.files.writeText("/workspace/a.txt", "hello");
    await sandbox.files.writeBytes("/workspace/a.bin", new Uint8Array([1, 2, 3]));
    const bytes: Uint8Array = await sandbox.files.readBytes("/workspace/a.bin");
    void bytes;
    await using download = await sandbox.files.readStream("/workspace/a.bin");
    for await (const chunk of download) void chunk;
  });

  const staticClient = new SandboxClient({
    baseUrl: "https://sandbox.example",
    credentials: new StaticToken("subject-token"),
  });
  await staticClient.close();
}

void consume;
void CommandFailedError;
void SandboxAbortedError;
