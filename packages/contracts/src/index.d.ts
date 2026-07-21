export declare const API_VERSION: "v1";
export declare const SANDBOX_PATH: "/v1/sandboxes";
export declare const MAX_JSON_BODY_BYTES: number;
export declare const SANDBOX_STATUS: Readonly<{
  READY: "ready";
  RELEASED: "released";
}>;

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

export interface AcquireSandboxResponse {
  sandbox: SandboxRecord;
  reused: boolean;
}

export interface ExecRequest {
  command: string;
  cwd?: string;
  env?: Record<string, string>;
  timeoutSeconds?: number;
}

export interface ExecResponse {
  stdout: string;
  stderr: string;
  code: number;
}

export interface WriteFileRequest {
  path: string;
  content: string;
  encoding?: FileEncoding;
}

export interface ReadFileResponse {
  path: string;
  content: string;
  encoding: FileEncoding;
}

export interface ErrorResponse {
  error: {
    code: string;
    message: string;
  };
}
