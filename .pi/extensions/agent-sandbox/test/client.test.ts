import assert from "node:assert/strict";
import test from "node:test";
import { redact, resolveSecretEnvironment } from "../client.js";

test("resolves secret references without putting values in parameters", () => {
  const result = resolveSecretEnvironment({ API_TOKEN: "HOST_API_TOKEN" }, { HOST_API_TOKEN: "super-secret" });
  assert.deepEqual(result.values, { API_TOKEN: "super-secret" });
  assert.deepEqual(result.secrets, ["super-secret"]);
});

test("redacts every secret occurrence and prefers longer secrets", () => {
  assert.equal(redact("token=abc123 abc", ["abc", "abc123"]), "token=[REDACTED] [REDACTED]");
});

test("rejects invalid variable names and missing host references", () => {
  assert.throws(() => resolveSecretEnvironment({ "BAD;NAME": "TOKEN" }, { TOKEN: "x" }), /Invalid sandbox/);
  assert.throws(() => resolveSecretEnvironment({ TOKEN: "MISSING" }, {}), /is not set/);
});
