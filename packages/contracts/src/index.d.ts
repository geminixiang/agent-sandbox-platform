export declare const API_VERSION: "v1";
export declare const LEASE_PATH: "/v1/leases";
export declare const MAX_JSON_BODY_BYTES: number;
export declare const LEASE_STATUS: Readonly<{
  ACTIVE: "active";
  RELEASED: "released";
  EXPIRED: "expired";
}>;

export type LeaseStatus = "active" | "released" | "expired";
export type FileEncoding = "utf8" | "base64";

export interface TenantScope {
  consumerId: string;
  subjectId: string;
}

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

export interface AcquireLeaseResponse {
  lease: LeaseRecord;
  replayed: boolean;
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
