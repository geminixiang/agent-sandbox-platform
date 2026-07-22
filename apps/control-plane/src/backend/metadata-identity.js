import { createHmac } from "node:crypto";

export function createMetadataIdentity(secret) {
  if (!secret) throw new TypeError("metadata secret is required");
  const digest = (...parts) =>
    createHmac("sha256", secret).update(JSON.stringify(parts)).digest("hex").slice(0, 40);
  return {
    consumerHash: (scope) => digest("consumer", scope.consumerId),
    scopeHash: (scope) => digest("scope", scope.consumerId, scope.subjectId),
    idempotencyHash: (scope, key) =>
      digest("idempotency", scope.consumerId, scope.subjectId, key),
  };
}
