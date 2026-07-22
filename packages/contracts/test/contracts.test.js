import assert from "node:assert/strict";
import test from "node:test";
import { API_VERSION, LEASE_PATH, LEASE_STATUS } from "../src/index.js";

test("exports the stable v1 lease protocol constants", () => {
  assert.equal(API_VERSION, "v1");
  assert.equal(LEASE_PATH, "/v1/leases");
  assert.deepEqual(LEASE_STATUS, {
    ACTIVE: "active",
    RELEASED: "released",
    EXPIRED: "expired",
  });
});
