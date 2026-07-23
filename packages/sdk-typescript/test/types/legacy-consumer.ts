import {
  createSubjectToken,
  SandboxPlatformClient,
  SandboxPlatformError,
  SandboxPlatformIntegrityError,
  type ExecResponse,
  type LeaseHandle,
  type LeasePage,
  type SandboxPlatformClientOptions,
} from "@geminixiang/sandbox-sdk";

async function legacy(): Promise<void> {
  const options: SandboxPlatformClientOptions = {
    baseUrl: "https://sandbox.example",
    consumerId: "consumer",
    subjectId: "subject",
    consumerSecret: "server-only-secret",
    tokenTtlSeconds: 60,
  };
  const client = new SandboxPlatformClient(options);
  const acquired = await client.acquire({ pool: "coding" }, { idempotencyKey: "request-1" });
  const lease: LeaseHandle = acquired.lease;
  const result: ExecResponse = await lease.exec("pwd", { timeoutSeconds: 5 });
  result.code satisfies number;
  await lease.writeFile("/workspace/a", "value", { encoding: "utf8" });
  const text: string = await lease.readFile("/workspace/a");
  void text;
  const page: LeasePage = await client.listPage();
  page.leases satisfies readonly LeaseHandle[];
  await lease.release();
  await client.close();
}

createSubjectToken({ consumerId: "c", subjectId: "s", consumerSecret: "x", expiresAt: 2_000_000_000 });
void legacy;
void SandboxPlatformError;
void SandboxPlatformIntegrityError;
