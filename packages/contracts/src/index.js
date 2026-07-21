export const API_VERSION = "v1";
export const SANDBOX_PATH = `/${API_VERSION}/sandboxes`;
export const MAX_JSON_BODY_BYTES = 1024 * 1024;

export const SANDBOX_STATUS = Object.freeze({
  READY: "ready",
  RELEASED: "released",
});
