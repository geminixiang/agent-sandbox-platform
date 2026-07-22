export const API_VERSION = "v1";
export const LEASE_PATH = `/${API_VERSION}/leases`;
export const MAX_JSON_BODY_BYTES = 1024 * 1024;

export const LEASE_STATUS = Object.freeze({
  ACTIVE: "active",
  RELEASED: "released",
  EXPIRED: "expired",
});
