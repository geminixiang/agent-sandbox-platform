import { createHmac, timingSafeEqual } from "node:crypto";

const TOKEN_VERSION = "v1";

export function signSubjectToken({ consumerId, subjectId, secret, expiresAt }) {
  requireIdentity(consumerId, "consumerId");
  requireIdentity(subjectId, "subjectId");
  if (!secret) throw new TypeError("secret is required");
  if (!Number.isInteger(expiresAt)) throw new TypeError("expiresAt must be an integer Unix timestamp");

  const payload = Buffer.from(JSON.stringify({ consumerId, subjectId, exp: expiresAt })).toString(
    "base64url",
  );
  const signed = `${TOKEN_VERSION}.${payload}`;
  return `${signed}.${signature(signed, secret)}`;
}

export function verifySubjectToken(
  token,
  resolveConsumerSecret,
  now = Date.now(),
  maxTokenTtlSeconds = 300,
) {
  if (typeof token !== "string") throw authError();
  const parts = token.split(".");
  if (parts.length !== 3 || parts[0] !== TOKEN_VERSION) throw authError();

  let claims;
  try {
    claims = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
  } catch {
    throw authError();
  }
  requireIdentity(claims.consumerId, "consumerId", authError);
  requireIdentity(claims.subjectId, "subjectId", authError);
  const nowSeconds = Math.floor(now / 1000);
  if (
    !Number.isInteger(claims.exp) ||
    claims.exp <= nowSeconds ||
    claims.exp > nowSeconds + maxTokenTtlSeconds
  ) {
    throw authError();
  }

  const secret = resolveConsumerSecret(claims.consumerId);
  if (!secret) throw authError();
  const expected = signature(`${parts[0]}.${parts[1]}`, secret);
  const actualBuffer = Buffer.from(parts[2]);
  const expectedBuffer = Buffer.from(expected);
  if (
    actualBuffer.length !== expectedBuffer.length ||
    !timingSafeEqual(actualBuffer, expectedBuffer)
  ) {
    throw authError();
  }
  return { consumerId: claims.consumerId, subjectId: claims.subjectId };
}

function signature(value, secret) {
  return createHmac("sha256", secret).update(value).digest("base64url");
}

function requireIdentity(value, name, errorFactory = (message) => new TypeError(message)) {
  if (typeof value !== "string" || !value.trim() || value.length > 200) {
    throw errorFactory(`${name} must be a non-empty string of at most 200 characters`);
  }
}

function authError() {
  return Object.assign(new Error("Invalid or expired subject token"), {
    status: 401,
    code: "UNAUTHORIZED",
  });
}
