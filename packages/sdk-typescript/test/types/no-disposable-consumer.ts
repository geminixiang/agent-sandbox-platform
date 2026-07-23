import {
  SandboxClient,
  StaticToken,
  type TokenProvider,
} from "@geminixiang/sandbox-sdk";

const provider: TokenProvider = ({ signal } = { signal: new AbortController().signal }) => {
  signal.throwIfAborted();
  return "subject-token";
};

async function consume(): Promise<void> {
  const client = new SandboxClient({
    baseUrl: "https://sandbox.example",
    credentials: provider,
  });
  const sandbox = await client.create({ pool: "coding" });
  await sandbox.close();
  await client.close();

  const staticClient = new SandboxClient({
    baseUrl: "https://sandbox.example",
    credentials: new StaticToken("subject-token"),
  });
  await staticClient.close();
}

void consume;
