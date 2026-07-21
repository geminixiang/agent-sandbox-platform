export type SandboxStatus = "ready" | "released";
export type FileEncoding = "utf8" | "base64";

export interface SandboxRecord {
  id: string;
  key: string;
  pool: string;
  status: SandboxStatus;
  createdAt: string;
  lastUsedAt: string;
}

export interface AcquireSandboxRequest {
  key: string;
  pool: string;
}

export interface ExecResponse {
  stdout: string;
  stderr: string;
  code: number;
}

export interface SandboxPlatformClientOptions {
  baseUrl: string | URL;
  token?: string;
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

export interface ExecOptions extends RequestOptions {
  cwd?: string;
  env?: Record<string, string>;
  timeoutSeconds?: number;
}

export interface FileOptions extends RequestOptions {
  encoding?: FileEncoding;
}

export declare class SandboxPlatformError extends Error {
  status?: number;
  code?: string;
  constructor(
    message: string,
    options?: { cause?: unknown; status?: number; code?: string },
  );
}

export declare class SandboxPlatformClient {
  constructor(options: SandboxPlatformClientOptions);
  acquire(
    request: AcquireSandboxRequest,
    options?: RequestOptions,
  ): Promise<{ sandbox: SandboxHandle; reused: boolean }>;
  get(id: string, options?: RequestOptions): Promise<SandboxHandle>;
}

export declare class SandboxHandle {
  readonly id: string;
  record: SandboxRecord;
  refresh(options?: RequestOptions): Promise<SandboxRecord>;
  exec(command: string, options?: ExecOptions): Promise<ExecResponse>;
  readFile(path: string, options?: FileOptions): Promise<string>;
  writeFile(path: string, content: string, options?: FileOptions): Promise<unknown>;
  release(options?: RequestOptions): Promise<SandboxRecord>;
  delete(options?: RequestOptions): Promise<void>;
}
