import {
  SandboxClient,
  type Sandbox,
} from "@geminixiang/sandbox-sdk";

export type SandboxTask = {
  id: string;
  pool: "coding" | "browser";
  command: string;
  cwd?: string;
  timeoutSeconds?: number;
  files?: ReadonlyArray<{ path: string; content: string }>;
};

export type SandboxTaskResult = {
  sandboxId: string;
  stdout: string;
  stderr: string;
  exitCode: number;
};

export class MikanSandboxExecutor {
  constructor(private readonly client: SandboxClient) {}

  async execute(task: SandboxTask, signal?: AbortSignal): Promise<SandboxTaskResult> {
    const sandbox = await this.client.create({
      pool: task.pool,
      idempotencyKey: task.id,
      signal,
    });
    try {
      await this.writeInputs(sandbox, task.files ?? [], signal);
      const result = await sandbox.run(task.command, {
        cwd: task.cwd,
        timeoutSeconds: task.timeoutSeconds,
        check: true,
        signal,
      });
      return {
        sandboxId: sandbox.id,
        stdout: result.stdout,
        stderr: result.stderr,
        exitCode: result.exitCode,
      };
    } finally {
      const cleanup = AbortSignal.timeout(10_000);
      await sandbox.close({ signal: cleanup });
    }
  }

  private async writeInputs(
    sandbox: Sandbox,
    files: ReadonlyArray<{ path: string; content: string }>,
    signal?: AbortSignal,
  ): Promise<void> {
    for (const file of files) {
      await sandbox.files.writeText(file.path, file.content, { signal });
    }
  }
}
