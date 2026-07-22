import assert from "node:assert/strict";
import test from "node:test";
import {
  API_VERSION,
  CONTENT_DIGEST_HEADER,
  DEFAULT_LIST_LIMIT,
  FILE_CONTENT_PATH_SUFFIX,
  LEASE_PATH,
  LEASE_STATUS,
  LIST_ERROR_CODE,
  MAX_FILE_TRANSFER_BYTES,
  MAX_LIST_LIMIT,
} from "../src/index.js";

test("exports the stable v1 lease protocol constants", () => {
  assert.equal(API_VERSION, "v1");
  assert.equal(LEASE_PATH, "/v1/leases");
  assert.deepEqual(LEASE_STATUS, {
    ACTIVE: "active",
    RELEASED: "released",
    EXPIRED: "expired",
  });
  assert.equal(FILE_CONTENT_PATH_SUFFIX, "/files/content");
  assert.equal(CONTENT_DIGEST_HEADER, "Content-Digest");
  assert.equal(MAX_FILE_TRANSFER_BYTES, 64 * 1024 * 1024);
  assert.equal(DEFAULT_LIST_LIMIT, 50);
  assert.equal(MAX_LIST_LIMIT, 100);
  assert.deepEqual(LIST_ERROR_CODE, {
    INVALID_CURSOR: "INVALID_CURSOR",
    CURSOR_EXPIRED: "CURSOR_EXPIRED",
    UNKNOWN_POOL: "UNKNOWN_POOL",
  });
});
