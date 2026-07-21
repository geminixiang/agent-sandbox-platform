import assert from "node:assert/strict";
import test from "node:test";
import { API_VERSION, SANDBOX_PATH, SANDBOX_STATUS } from "../src/index.js";

test("exports the stable v1 protocol constants", () => {
  assert.equal(API_VERSION, "v1");
  assert.equal(SANDBOX_PATH, "/v1/sandboxes");
  assert.deepEqual(SANDBOX_STATUS, { READY: "ready", RELEASED: "released" });
});
