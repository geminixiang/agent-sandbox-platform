declare global {
  interface SymbolConstructor {
    readonly asyncDispose: unique symbol;
  }
}

export type LeaseStatus = "active" | "released" | "expired";
export type FileEncoding = "utf8" | "base64";

export interface LeaseRecord {
  readonly id: string;
  readonly pool: string;
  readonly status: LeaseStatus;
  readonly createdAt: string;
  readonly expiresAt: string;
  readonly lastUsedAt: string;
}

export interface TokenProviderContext {
  readonly signal: AbortSignal;
}

export type TokenProvider = (context?: TokenProviderContext) => string | PromiseLike<string>;

export interface TokenProviderObject {
  getToken(context?: TokenProviderContext): string | PromiseLike<string>;
}

export type Credentials = TokenProvider | TokenProviderObject;

export interface SandboxClientOptions {
  baseUrl: string | URL;
  credentials: Credentials;
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
}

/** @deprecated Prefer SandboxClientOptions with a short-lived token provider. */
export interface LegacySandboxPlatformClientOptions {
  baseUrl: string | URL;
  consumerId: string;
  subjectId: string;
  consumerSecret: string;
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
  tokenTtlSeconds?: number;
  credentials?: never;
}

/** @deprecated Use LegacySandboxPlatformClientOptions. */
export type SandboxPlatformClientOptions = LegacySandboxPlatformClientOptions;

export interface OperationOptions {
  signal?: AbortSignal;
  timeoutMs?: number;
}

/** @deprecated Use OperationOptions. */
export type RequestOptions = OperationOptions;

export interface CreateSandboxOptions extends OperationOptions {
  pool: string;
  ttlSeconds?: number;
  idempotencyKey?: string;
}

/** @deprecated Use CreateSandboxOptions. */
export interface AcquireLeaseRequest {
  pool: string;
  ttlSeconds?: number;
}

/** @deprecated Use CreateSandboxOptions. */
export interface AcquireOptions extends OperationOptions {
  idempotencyKey?: string;
}

export interface ListOptions extends OperationOptions {
  pool?: string;
  limit?: number;
  cursor?: string;
}

export interface RunOptions extends OperationOptions {
  cwd?: string;
  env?: Readonly<Record<string, string>>;
  timeoutSeconds?: number;
  check?: boolean;
}

/** @deprecated Use RunOptions. */
export type ExecOptions = RunOptions;

export interface FileOperationOptions extends OperationOptions {}

/** @deprecated Use FileOperationOptions. */
export interface FileOptions extends OperationOptions {
  encoding?: FileEncoding;
}

export interface StreamWriteOptions extends OperationOptions {
  sizeBytes: number;
  /** Lowercase 64-character SHA-256 hexadecimal digest. */
  sha256: string;
}

/** @deprecated Use CommandResult. */
export interface ExecResponse {
  readonly stdout: string;
  readonly stderr: string;
  readonly code: number;
}

export declare class SandboxError extends Error {
  readonly status?: number;
  readonly code?: string;
  constructor(message: string, options?: { cause?: unknown; status?: number; code?: string });
}

/** @deprecated Use SandboxError. */
export { SandboxError as SandboxPlatformError };

export declare class CommandFailedError extends SandboxError {
  readonly command: string;
  readonly result: CommandResult;
  constructor(command: string, result: CommandResult);
}

export declare class SandboxNotFoundError extends SandboxError {}
export declare class SandboxNotActiveError extends SandboxError {}
export declare class SandboxExpiredError extends SandboxNotActiveError {}
export declare class SandboxQuotaExceededError extends SandboxError {}
export declare class SandboxAbortedError extends SandboxError {}
export declare class SandboxFileNotFoundError extends SandboxError {}
export declare class SandboxTransferTooLargeError extends SandboxError {}
export declare class SandboxTransferLimitError extends SandboxError {}
export declare class SandboxIntegrityError extends SandboxError {}
export declare class SandboxStreamingNotSupportedError extends SandboxError {}
export declare class SandboxInvalidCursorError extends SandboxError {}
export declare class SandboxCursorExpiredError extends SandboxError {}
export declare class SandboxUnknownPoolError extends SandboxError {}

/** @deprecated Use SandboxIntegrityError. */
export { SandboxIntegrityError as SandboxPlatformIntegrityError };

export declare class StaticToken implements TokenProviderObject {
  readonly token: string;
  constructor(token: string);
  getToken(): string;
}

export declare class CommandResult {
  readonly stdout: string;
  readonly stderr: string;
  readonly exitCode: number;
  /** @deprecated Use exitCode. */
  readonly code: number;
  readonly succeeded: boolean;
  constructor(value: { stdout: string; stderr: string; code: number });
}

export declare class SandboxPage {
  readonly sandboxes: readonly Sandbox[];
  readonly nextCursor: string | null;
  /** @deprecated Use sandboxes. */
  readonly leases: readonly Sandbox[];
  constructor(sandboxes: Iterable<Sandbox>, nextCursor: string | null);
}

/** @deprecated Use SandboxPage. */
export type LeasePage = SandboxPage;

export declare class SandboxClient {
  constructor(options: SandboxClientOptions | LegacySandboxPlatformClientOptions);
  close(): Promise<void>;
  [Symbol.asyncDispose](): Promise<void>;
  create(options: CreateSandboxOptions): Promise<Sandbox>;
  sandbox<T>(options: CreateSandboxOptions, callback: (sandbox: Sandbox) => T | PromiseLike<T>): Promise<T>;
  /** @deprecated Use create. */
  acquire(
    request: AcquireLeaseRequest,
    options?: AcquireOptions,
  ): Promise<{
    readonly lease: Sandbox;
    readonly replayed: boolean;
    readonly idempotencyKey: string;
  }>;
  listPage(options?: ListOptions): Promise<SandboxPage>;
  list(options?: ListOptions): AsyncIterable<Sandbox>;
  connect(id: string, options?: OperationOptions): Promise<Sandbox>;
  get(id: string, options?: OperationOptions): Promise<Sandbox>;
  /** @deprecated Tokens are supplied by credentials. */
  subjectToken(): string;
}

/** @deprecated Use SandboxClient. */
export { SandboxClient as SandboxPlatformClient };

export declare class Sandbox {
  readonly id: string;
  readonly record: LeaseRecord;
  readonly files: SandboxFiles;
  refresh(options?: OperationOptions): Promise<LeaseRecord>;
  run(command: string, options?: RunOptions): Promise<CommandResult>;
  /** @deprecated Use run. */
  exec(command: string, options?: ExecOptions): Promise<ExecResponse>;
  /** @deprecated Use files.readText or files.readBytes. */
  readFile(path: string, options?: FileOptions): Promise<string>;
  /** @deprecated Use files.writeText or files.writeBytes. */
  writeFile(path: string, content: string, options?: FileOptions): Promise<void>;
  /** @deprecated Use files.readStream. */
  readFileStream(path: string, options?: OperationOptions): Promise<FileDownload>;
  /** @deprecated Use files.writeStream. */
  writeFileStream(
    path: string,
    chunks: AsyncIterable<Uint8Array>,
    options: StreamWriteOptions,
  ): Promise<void>;
  release(options?: OperationOptions): Promise<LeaseRecord>;
  delete(options?: OperationOptions): Promise<void>;
  close(options?: OperationOptions): Promise<void>;
  [Symbol.asyncDispose](): Promise<void>;
}

/** @deprecated Use Sandbox. */
export { Sandbox as LeaseHandle };

export declare class SandboxFiles {
  readText(path: string, options?: FileOperationOptions): Promise<string>;
  writeText(path: string, content: string, options?: FileOperationOptions): Promise<void>;
  readBytes(path: string, options?: FileOperationOptions): Promise<Uint8Array>;
  writeBytes(path: string, content: Uint8Array, options?: FileOperationOptions): Promise<void>;
  readStream(path: string, options?: FileOperationOptions): Promise<FileDownload>;
  writeStream(
    path: string,
    chunks: AsyncIterable<Uint8Array>,
    options: StreamWriteOptions,
  ): Promise<void>;
  /** @deprecated Use readText or readBytes. */
  readFile(path: string, options?: FileOptions): Promise<string>;
  /** @deprecated Use writeText or writeBytes. */
  writeFile(path: string, content: string, options?: FileOptions): Promise<void>;
  /** @deprecated Use readStream. */
  readFileStream(path: string, options?: FileOperationOptions): Promise<FileDownload>;
  /** @deprecated Use writeStream. */
  writeFileStream(
    path: string,
    chunks: AsyncIterable<Uint8Array>,
    options: StreamWriteOptions,
  ): Promise<void>;
}

export declare class FileDownload implements AsyncIterable<Uint8Array> {
  readonly sizeBytes: number;
  /** Lowercase 64-character SHA-256 hexadecimal digest. */
  readonly sha256: string;
  [Symbol.asyncIterator](): AsyncIterator<Uint8Array>;
  close(): Promise<void>;
  [Symbol.asyncDispose](): Promise<void>;
}

export declare function createSubjectToken(options: {
  consumerId: string;
  subjectId: string;
  consumerSecret: string;
  expiresAt: number;
}): string;
