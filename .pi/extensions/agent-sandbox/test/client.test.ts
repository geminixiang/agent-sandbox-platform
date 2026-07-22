import assert from "node:assert/strict";
import test from "node:test";
import { createSubjectToken, redact, resolveSecretEnvironment, summarizeBase64 } from "../client.js";

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

test("summarizes base64 without returning binary content by default", () => {
  const content = Buffer.from("hello").toString("base64");
  assert.deepEqual(summarizeBase64("/workspace/a.bin", content), {
    path: "/workspace/a.bin",
    encoding: "base64",
    bytes: 5,
    base64Characters: 8,
    sha256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
  });
  assert.equal(summarizeBase64("/workspace/a.bin", content, true).content, content);
});
test("redacts every secret occurrence and prefers longer secrets", () => {
  assert.equal(redact("token=abc123 abc", ["abc", "abc123"]), "token=[REDACTED] [REDACTED]");
});

test("rejects invalid variable names and missing host references", () => {
  assert.throws(() => resolveSecretEnvironment({ "BAD;NAME": "TOKEN" }, { TOKEN: "x" }), /Invalid sandbox/);
  assert.throws(() => resolveSecretEnvironment({ TOKEN: "MISSING" }, {}), /is not set/);
});
