export type LeaseStatus = "active" | "released" | "expired";
export type FileEncoding = "utf8" | "base64";

export interface LeaseRecord {
  id: string;
  pool: string;
  status: LeaseStatus;
  createdAt: string;
  expiresAt: string;
  lastUsedAt: string;
}

export interface AcquireLeaseRequest {
  pool: string;
  ttlSeconds?: number;
}

export interface SandboxPlatformClientOptions {
  baseUrl: string | URL;
  consumerId: string;
  subjectId: string;
  consumerSecret: string;
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
  tokenTtlSeconds?: number;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

export interface AcquireOptions extends RequestOptions {
  idempotencyKey?: string;
}

export interface ExecOptions extends RequestOptions {
  cwd?: string;
  env?: Record<string, string>;
  timeoutSeconds?: number;
}

export interface FileOptions extends RequestOptions {
  encoding?: FileEncoding;
}

export interface ExecResponse {
  stdout: string;
  stderr: string;
  code: number;
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
    request: AcquireLeaseRequest,
    options?: AcquireOptions,
  ): Promise<{ lease: LeaseHandle; replayed: boolean; idempotencyKey: string }>;
  get(id: string, options?: RequestOptions): Promise<LeaseHandle>;
}

export declare class LeaseHandle {
  readonly id: string;
  record: LeaseRecord;
  refresh(options?: RequestOptions): Promise<LeaseRecord>;
  exec(command: string, options?: ExecOptions): Promise<ExecResponse>;
  readFile(path: string, options?: FileOptions): Promise<string>;
  writeFile(path: string, content: string, options?: FileOptions): Promise<unknown>;
  release(options?: RequestOptions): Promise<LeaseRecord>;
  delete(options?: RequestOptions): Promise<void>;
}

export declare function createSubjectToken(options: {
  consumerId: string;
  subjectId: string;
  consumerSecret: string;
  expiresAt: number;
}): string;
