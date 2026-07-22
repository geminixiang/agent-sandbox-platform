export const API_VERSION = "v1";
export const LEASE_PATH = `/${API_VERSION}/leases`;
export const MAX_JSON_BODY_BYTES = 1024 * 1024;
export const DEFAULT_LIST_LIMIT = 50;
export const MAX_LIST_LIMIT = 100;
export const FILE_CONTENT_PATH_SUFFIX = "/files/content";
export const CONTENT_DIGEST_HEADER = "Content-Digest";
export const MAX_FILE_TRANSFER_BYTES = 64 * 1024 * 1024;

export const LEASE_STATUS = Object.freeze({
  ACTIVE: "active",
  RELEASED: "released",
  EXPIRED: "expired",
});

export const LIST_ERROR_CODE = Object.freeze({
  INVALID_CURSOR: "INVALID_CURSOR",
  CURSOR_EXPIRED: "CURSOR_EXPIRED",
  UNKNOWN_POOL: "UNKNOWN_POOL",
});
