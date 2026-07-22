import assert from "node:assert/strict";
import test from "node:test";
import { createSubjectToken, redact, resolveSecretEnvironment } from "../client.js";

test("creates a verifiable short-lived local Subject token", () => {
  const token = createSubjectToken({ baseUrl: "http://127.0.0.1:8787", consumerId: "pi", subjectId: "session", consumerSecret: "secret" }, 2_000_000_000);
  assert.match(token, /^v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
  assert.equal(token.includes("secret"), false);
});
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
