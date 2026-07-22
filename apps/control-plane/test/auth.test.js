import assert from "node:assert/strict";
import test from "node:test";
import { signSubjectToken, verifySubjectToken } from "../src/auth.js";

const claims = {
  consumerId: "mikan",
  subjectId: "subject-a",
  secret: "secret-a",
  expiresAt: 1_900_000_300,
};

test("verifies a signed tenant scope", () => {
  const token = signSubjectToken(claims);
  assert.deepEqual(
    verifySubjectToken(token, (consumerId) => (consumerId === "mikan" ? "secret-a" : undefined), 1_900_000_000_000),
    { consumerId: "mikan", subjectId: "subject-a" },
  );
});

test("rejects tampered, expired, and unknown-consumer tokens identically", () => {
  const token = signSubjectToken(claims);
  const tokens = [
    `${token.slice(0, -1)}x`,
    signSubjectToken({ ...claims, expiresAt: 1_899_999_999 }),
    signSubjectToken({ ...claims, expiresAt: 1_900_000_301 }),
    signSubjectToken({ ...claims, consumerId: "unknown" }),
  ];
  for (const candidate of tokens) {
    assert.throws(
      () => verifySubjectToken(candidate, (consumerId) => (consumerId === "mikan" ? "secret-a" : undefined), 1_900_000_000_000),
      (error) => {
        assert.equal(error.status, 401);
        assert.equal(error.code, "UNAUTHORIZED");
        assert.equal(error.message, "Invalid or expired subject token");
        return true;
      },
    );
  }
});
