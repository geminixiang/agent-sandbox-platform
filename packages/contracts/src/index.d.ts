export declare const API_VERSION: "v1";
export declare const LEASE_PATH: "/v1/leases";
export declare const MAX_JSON_BODY_BYTES: number;
export declare const FILE_CONTENT_PATH_SUFFIX: "/files/content";
export declare const CONTENT_DIGEST_HEADER: "Content-Digest";
export declare const MAX_FILE_TRANSFER_BYTES: 67108864;
export declare const LEASE_STATUS: Readonly<{
  ACTIVE: "active";
  RELEASED: "released";
  EXPIRED: "expired";
}>;

export type LeaseStatus = "active" | "released" | "expired";
export type FileEncoding = "utf8" | "base64";
export type FileTransferErrorCode =
  | "INVALID_REQUEST"
  | "INVALID_CONTENT_DIGEST"
  | "CONTENT_LENGTH_MISMATCH"
  | "LENGTH_REQUIRED"
  | "TRANSFER_TOO_LARGE"
  | "UNSUPPORTED_MEDIA_TYPE"
  | "CONTENT_DIGEST_MISMATCH"
  | "STREAMING_NOT_SUPPORTED";

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

export interface FileTransferMetadata {
  sizeBytes: number;
  /** Lowercase 64-character SHA-256 hexadecimal digest. */
  sha256: string;
}

export interface ErrorResponse {
  error: {
    code: string;
    message: string;
  };
}
